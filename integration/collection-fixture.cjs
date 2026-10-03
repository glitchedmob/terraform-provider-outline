// SPDX-License-Identifier: MPL-2.0

// Test-only document seeding/inspection, empty collection archive metadata,
// and a second workspace for authorization. CRUD and grants use the HTTP API.
const { version } = require("./package.json");
const { sequelize } = require("./build/server/storage/database");
const { Team, User, ApiKey, Collection, Document, UserMembership, GroupMembership } = require("./build/server/models");

async function fixture() {
  const [action, collectionId] = process.argv.slice(2);
  if (version !== "1.10.1" || !["seed", "deleted", "workspace", "archive", "restore"].includes(action)) {
    throw new Error("Collection fixture requires Outline 1.10.1 and a known fixture action");
  }
  if (["archive", "restore"].includes(action) &&
      (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(collectionId || "") ||
       collectionId === "00000000-0000-0000-0000-000000000000")) {
    throw new Error("Archive fixture requires a canonical nonzero collection UUID");
  }
  await sequelize.authenticate();
  if ((await Team.count({ paranoid: false })) !== 1) {
    throw new Error("Collection fixture requires one disposable acceptance team");
  }
  const team = await Team.findOne({ where: { name: "Terraform acceptance" } });
  const admin = team && await User.findOne({ where: {
    teamId: team.id, email: "terraform-admin@example.invalid", role: "admin", suspendedAt: null
  } });
  if (!team || !admin) {
    throw new Error("Refusing to change a database outside the disposable acceptance workspace");
  }
  let result;
  if (action === "workspace") {
    result = await sequelize.transaction(async (transaction) => {
      const other = await Team.create({ name: "Terraform collection foreign workspace" }, { transaction });
      const user = await User.create({
        name: "Terraform collection foreign admin", email: "terraform-foreign@example.invalid",
        role: "admin", teamId: other.id, lastSignedInAt: new Date()
      }, { transaction });
      const key = await ApiKey.create({ name: "Terraform collection foreign key", userId: user.id, scope: null }, { transaction });
      if (!key.value || key.scope !== null) {
        throw new Error("ORM did not create an unrestricted foreign API key");
      }
      return { api_key: key.value };
    });
  } else if (["archive", "restore"].includes(action)) {
    result = await sequelize.transaction(async (transaction) => {
      const collection = await Collection.findByPk(collectionId, {
        paranoid: false, transaction, lock: transaction.LOCK.UPDATE
      });
      if (!collection || collection.teamId !== team.id || collection.createdById !== admin.id ||
          !["Terraform collection release duplicate", "Terraform collection release managed"].includes(collection.name) ||
          collection.deletedAt || collection.permission !== null ||
          !!collection.archivedAt !== (action === "restore") ||
          (action === "restore" && collection.archivedById !== admin.id) ||
          (await Document.unscoped().count({ where: { collectionId }, paranoid: false, transaction })) !== 0) {
        throw new Error("Refusing to archive or restore outside the empty collection release fixture");
      }
      // This test needs only archive metadata, not document lifecycle or queue
      // events. Never call an APIContext-dependent model method with a fake ctx.
      collection.archivedAt = action === "archive" ? new Date() : null;
      collection.archivedById = action === "archive" ? admin.id : null;
      await collection.save({ fields: ["archivedAt", "archivedById"], hooks: false, transaction });
      return { collection_id: collection.id, deleted: false, archived: !!collection.archivedAt };
    });
  } else {
    const collection = await Collection.findByPk(collectionId, { paranoid: false });
    if (!collection || collection.teamId !== team.id ||
        collection.name !== "Terraform collection deletion semantics" || collection.archivedAt ||
        !!collection.deletedAt !== (action === "deleted")) {
      throw new Error("Refusing to seed or inspect a collection outside this deletion fixture");
    }
    const prefix = `Terraform collection fixture ${collection.id} `;
    if (action === "seed") {
      if (await Document.unscoped().count({ where: { collectionId }, paranoid: false })) {
        throw new Error("Refusing to seed a collection that already has documents");
      }
      // Document's after-create hook opens its own transaction to update the
      // collection structure. An outer transaction would deadlock that hook.
      for (const kind of ["published", "draft", "archived"]) {
        await Document.create({
          title: prefix + kind, text: "Acceptance fixture content", teamId: team.id,
          collectionId, createdById: admin.id, lastModifiedById: admin.id,
          publishedAt: kind === "draft" ? null : new Date(),
          archivedAt: kind === "archived" ? new Date() : null
        });
      }
    }
    // The application worker detaches drafts after collections.delete commits.
    // Poll the ORM, never invoke the queue task from this fixture.
    const deadline = Date.now() + 30000;
    let documents;
    do {
      documents = await Document.unscoped().findAll({
        where: { teamId: team.id, title: ["published", "draft", "archived"].map((kind) => prefix + kind) },
        paranoid: false, order: [["title", "ASC"]]
      });
      if (action !== "deleted" || documents.some((doc) => doc.title === prefix + "draft" && doc.collectionId === null)) {
        break;
      }
      await new Promise((resolve) => setTimeout(resolve, 500));
    } while (Date.now() < deadline);
    if (documents.length !== 3) {
      throw new Error("Deletion fixture must still have all three document rows");
    }
    const users = await UserMembership.findAll({ where: { collectionId }, order: [["id", "ASC"]] });
    const groups = await GroupMembership.findAll({ where: { collectionId }, paranoid: false, order: [["id", "ASC"]] });
    result = {
      collection_id: collection.id, deleted: !!collection.deletedAt,
      documents: documents.map((doc) => ({
        id: doc.id, kind: doc.title.slice(prefix.length), collection_id: doc.collectionId,
        published: !!doc.publishedAt, archived: !!doc.archivedAt, deleted: !!doc.deletedAt,
        deleted_by_id: doc.deletedById
      })),
      user_memberships: users.map((row) => ({ id: row.id, user_id: row.userId, permission: row.permission })),
      group_memberships: groups.map((row) => ({ id: row.id, group_id: row.groupId, permission: row.permission, deleted: !!row.deletedAt }))
    };
  }
  await sequelize.close();
  process.stdout.write(`OUTLINE_ACCEPTANCE_COLLECTION=${JSON.stringify({ version, ...result })}\n`);
  // Outline model imports also start Redis clients. This fixture runs once.
  process.exit(0);
}

fixture().catch((error) => {
  console.error(error);
  process.exit(1);
});
