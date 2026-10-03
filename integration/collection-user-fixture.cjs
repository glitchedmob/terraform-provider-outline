// SPDX-License-Identifier: MPL-2.0

// Only empty-collection archive metadata and the viewer export preference use
// the ORM. User grants and group memberships always use the released HTTP API.
const { version } = require("./package.json");
const { sequelize } = require("./build/server/storage/database");
const { Team, User, Collection, Document } = require("./build/server/models");
const { TeamPreference } = require("./build/shared/types");

async function fixture() {
  const [action, id] = process.argv.slice(2);
  const archive = ["archive", "restore"].includes(action);
  if (version !== "1.10.1" ||
      !["archive", "restore", "export-on", "export-off", "export-reset"].includes(action) ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(id || "") ||
      id === "00000000-0000-0000-0000-000000000000") {
    throw new Error("Collection user fixture requires Outline 1.10.1, a known action, and a canonical nonzero UUID");
  }
  await sequelize.authenticate();
  if ((await Team.count({ paranoid: false })) !== 1) {
    throw new Error("Collection user fixture requires one disposable acceptance team");
  }
  const result = await sequelize.transaction(async (transaction) => {
    const team = await Team.findOne({
      where: { name: "Terraform acceptance" }, transaction, lock: transaction.LOCK.UPDATE
    });
    const admin = team && await User.findOne({ where: {
      teamId: team.id, email: "terraform-admin@example.invalid", role: "admin", suspendedAt: null
    }, transaction });
    const collection = await Collection.findByPk(id, {
      paranoid: false, transaction, lock: transaction.LOCK.UPDATE
    });
    if (!team || !admin || !collection || collection.teamId !== team.id ||
        collection.createdById !== admin.id || collection.deletedAt ||
        collection.name !== (archive ? "Terraform collection user archived" : "Terraform collection user target roles") ||
        (await Document.unscoped().count({ where: { collectionId: id }, paranoid: false, transaction })) !== 0) {
      throw new Error("Refusing to change metadata outside the empty collection user acceptance fixture");
    }
    if (archive) {
      if (collection.permission !== null || !!collection.archivedAt !== (action === "restore") ||
          (action === "restore" && collection.archivedById !== admin.id)) {
        throw new Error("Refusing to change unexpected collection archive metadata");
      }
      collection.archivedAt = action === "archive" ? new Date() : null;
      collection.archivedById = action === "archive" ? admin.id : null;
      await collection.save({ fields: ["archivedAt", "archivedById"], hooks: false, transaction });
      return { collection_id: id, archived: !!collection.archivedAt };
    }
    if (collection.archivedAt || ![null, "read_write"].includes(collection.permission)) {
      throw new Error("Refusing to change preferences with an unexpected role-limit collection");
    }
    const preference = TeamPreference.ViewersCanExport;
    const previous = team.preferences?.[preference] ?? null;
    if (previous !== null && typeof previous !== "boolean") {
      throw new Error("Refusing to overwrite a non-boolean export preference");
    }
    if (action === "export-reset") {
      const preferences = { ...team.preferences };
      delete preferences[preference];
      team.preferences = preferences;
    } else {
      team.setPreference(preference, action === "export-on");
    }
    await team.save({ fields: ["preferences"], transaction });
    return { collection_id: id, previous_export: previous, viewers_can_export: !!team.getPreference(preference) };
  });
  await sequelize.close();
  process.stdout.write(`OUTLINE_ACCEPTANCE_COLLECTION_USER=${JSON.stringify({ version, action, ...result })}\n`);
  process.exit(0);
}

fixture().catch((error) => {
  console.error(error);
  process.exit(1);
});
