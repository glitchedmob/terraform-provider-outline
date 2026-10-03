// SPDX-License-Identifier: MPL-2.0

// Large lists and otherwise unreachable conflicting pairs use the released
// image's ORM. Every assertion reads the real HTTP API. No schema changes.
const { randomUUID } = require("crypto");
const { Op } = require("sequelize");
const { version } = require("./package.json");
const { sequelize } = require("./build/server/storage/database");
const { Team, User, Group, Collection, Document, UserMembership, GroupMembership, GroupUser } = require("./build/server/models");

const sharedName = "Grant refresh shared %_\\ name";
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const actions = ["seed", "duplicate-user", "duplicate-group", "remove-duplicate-user", "remove-duplicate-group", "archive", "restore"];

async function fixture() {
  const [action, collectionId, groupId, targetId] = process.argv.slice(2);
  if (version !== "1.10.1" || !actions.includes(action) ||
      [collectionId, groupId, ...(targetId ? [targetId] : [])].some((id) => !uuid.test(id) || id === "00000000-0000-0000-0000-000000000000") ||
      ((action.includes("duplicate")) !== !!targetId)) {
    throw new Error("Grant refresh fixture requires Outline 1.10.1, a known action, and canonical nonzero UUIDs");
  }
  await sequelize.authenticate();
  if ((await Team.count({ paranoid: false })) !== 1) {
    throw new Error("Grant refresh fixture requires one disposable acceptance team");
  }
  const result = await sequelize.transaction(async (transaction) => {
    const team = await Team.findOne({ where: { name: "Terraform acceptance" }, transaction });
    const admin = team && await User.findOne({ where: {
      teamId: team.id, email: "terraform-admin@example.invalid", role: "admin", suspendedAt: null
    }, transaction });
    const collection = await Collection.findByPk(collectionId, { paranoid: false, transaction, lock: transaction.LOCK.UPDATE });
    const group = await Group.findByPk(groupId, { transaction, lock: transaction.LOCK.UPDATE });
    if (!team || !admin || !collection || !group || collection.teamId !== team.id || group.teamId !== team.id ||
        collection.createdById !== admin.id || group.createdById !== admin.id || collection.deletedAt ||
        collection.name !== "Terraform grant refresh query" || collection.permission !== null ||
        group.name !== "Terraform group member grant refresh query" || group.externalId ||
        (await Document.unscoped().count({ where: { collectionId }, paranoid: false, transaction })) !== 0) {
      throw new Error("Refusing to change a database outside the empty grant refresh acceptance parents");
    }
    if (["archive", "restore"].includes(action)) {
      if (!!collection.archivedAt !== (action === "restore") ||
          (action === "restore" && collection.archivedById !== admin.id)) {
        throw new Error("Unexpected archive metadata in grant refresh fixture");
      }
      collection.archivedAt = action === "archive" ? new Date() : null;
      collection.archivedById = action === "archive" ? admin.id : null;
      await collection.save({ fields: ["archivedAt", "archivedById"], hooks: false, transaction });
      return { archived: !!collection.archivedAt };
    }
    if (collection.archivedAt) {
      throw new Error("Refusing to seed or corrupt an archived collection");
    }
    if (action !== "seed") {
      const userGrant = action.endsWith("user");
      const Model = userGrant ? UserMembership : GroupMembership;
      const principalField = userGrant ? "userId" : "groupId";
      const Principal = userGrant ? User : Group;
      if (action.startsWith("remove-")) {
        const duplicate = await Model.findByPk(targetId, { transaction });
        const principal = duplicate && await Principal.findByPk(duplicate[principalField], { transaction });
        if (!duplicate || !principal || principal.teamId !== team.id || principal.name !== sharedName ||
            duplicate.collectionId !== collectionId || duplicate.documentId || duplicate.sourceId ||
            duplicate.createdById !== admin.id || duplicate.permission !== "admin" ||
            duplicate.createdAt.toISOString() !== "2000-01-01T00:00:00.000Z") {
          throw new Error("Refusing to remove a grant not created by the duplicate fixture");
        }
        await duplicate.destroy({ hooks: false, force: true, transaction });
        return { removed_id: targetId };
      }
      const principal = await Principal.findByPk(targetId, { transaction });
      const existing = await Model.findAll({ where: { collectionId, [principalField]: targetId }, transaction });
      if (!principal || principal.teamId !== team.id || principal.name !== sharedName ||
          (userGrant && !principal.email.startsWith("grant-refresh-")) ||
          existing.length !== 1 || existing[0].permission !== "read_write" || existing[0].documentId || existing[0].sourceId) {
        throw new Error("Refusing to duplicate a pair outside the shared-name grant refresh fixture");
      }
      // Both pinned collection-pair indexes are nonunique. Put the conflicting
      // row on page two, after the first-page target, with a distinct row UUID.
      const duplicate = await Model.create({
        id: randomUUID(), collectionId, [principalField]: targetId, permission: "admin",
        createdById: admin.id, documentId: null, sourceId: null,
        createdAt: new Date("2000-01-01T00:00:00.000Z")
      }, { hooks: false, transaction });
      return { duplicate_id: duplicate.id };
    }
    if ((await User.count({ where: { teamId: team.id, email: { [Op.like]: "grant-refresh-%@example.invalid" } }, transaction })) ||
        (await GroupMembership.count({ where: { collectionId }, transaction })) ||
        (await GroupUser.count({ where: { groupId }, transaction }))) {
      throw new Error("Refusing to seed the grant refresh fixture twice");
    }
    const cases = {};
    let sequence = 0;
    async function add(label, name, member = true, permission = "read") {
      const createdAt = new Date(Date.UTC(2020, 0, 1) + sequence * 1000);
      const user = await User.create({
        name, email: `grant-refresh-${sequence++}@example.invalid`, role: "member", teamId: team.id,
        lastSignedInAt: new Date()
      }, { transaction });
      const principalGroup = await Group.create({ name, teamId: team.id, createdById: admin.id }, { transaction });
      if (member) {
        await UserMembership.create({
          id: randomUUID(), collectionId, userId: user.id, permission, createdById: admin.id,
          documentId: null, sourceId: null, createdAt
        }, { hooks: false, transaction });
        await GroupMembership.create({
          id: randomUUID(), collectionId, groupId: principalGroup.id, permission, createdById: admin.id,
          documentId: null, sourceId: null, createdAt
        }, { hooks: false, transaction });
        await GroupUser.create({
          groupId, userId: user.id, permission: permission === "read_write" ? "admin" : "member",
          createdById: admin.id, createdAt
        }, { hooks: false, transaction });
      }
      if (label) cases[label] = { name, user_id: user.id, group_id: principalGroup.id };
    }
    await add("percent", "Grant refresh percent%leaf");
    await add("percent_decoy", "Grant refresh percentWILDCARDleaf");
    await add("underscore", "Grant refresh underscore_leaf");
    await add("underscore_decoy", "Grant refresh underscoreXleaf");
    await add("backslash", "Grant refresh backslash\\leaf");
    await add("backslash_decoy", "Grant refresh backslashleaf");
    await add("rename", "Grant refresh rename before");
    await add("external", "Terraform collection group synchronized");
    for (let i = 0; i < 102; i++) {
      await add(i === 100 ? "shared" : null, sharedName, true, i === 100 ? "read_write" : "read");
    }
    await add("absent", "Grant refresh absent", false);
    await add("same_name_absent", sharedName, false);
    return { cases };
  });
  await sequelize.close();
  process.stdout.write(`OUTLINE_ACCEPTANCE_GRANT_REFRESH=${JSON.stringify({ version, action, collection_id: collectionId, group_id: groupId, ...result })}\n`);
  process.exit(0);
}

fixture().catch((error) => {
  console.error(error);
  process.exit(1);
});
