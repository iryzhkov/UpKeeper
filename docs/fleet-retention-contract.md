# Fleet retention recovery contract

Retained prior.json is an object containing only a nonempty documents list. Each document names a unique fleet-owned destination, an actual boolean existed, and content. The complete record is validated and all present content decoded before any destination or ownership receipt is changed. Single-document restoration validates the other entries too. Retry records are checked before forward ownership preparation or activation, and combined retirement checks its existing records before effects.

Present documents accept historical strings without encoding or canonical strict base64/v1 strings. Absent documents accept null content without encoding, historical empty strings, or canonical encoded empty strings; nonempty content contradicts absence. Missing content or existence, null/nonboolean existence, unknown fields/encodings, invalid base64, duplicate destinations and unowned paths refuse. Guard metadata is never part of the retained format: trusted runtime callers pass the expected live bytes and guard flag separately. Descriptor-relative no-follow activation and drift checks remain in force.

Format refusals preserve live bytes, absence and ownership receipts and do not report successful rollback. This validates data before replay; filesystem failures during replay remain errors and do not promise an all-destination atomic transaction.
