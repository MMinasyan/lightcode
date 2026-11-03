// The exact-JSON text helpers: parse source access plus JSON.rawJSON keep every
// numeric lexeme exactly as carried, and object edits are local member
// replacement over that parser.
import type { JsonObject } from '../generated/protocol/types.gen';

// Parse one document, replacing every number with its exact source token.
export function parseExactJSON(text: string): unknown {
  return JSON.parse(text, (_key, value, context) => {
    if (typeof value === 'number' && context !== undefined && typeof context.source === 'string') {
      return JSON.rawJSON(context.source);
    }
    return value;
  });
}

// Presentation-only numeric interpretation at the typed display boundaries:
// one exact raw token becomes its ordinary Number value, and every other value
// (including an ordinary object with a rawJSON member) is returned unchanged.
export function presentNumber(value: unknown): unknown {
  if (JSON.isRawJSON(value)) {
    return Number((value as { rawJSON: string }).rawJSON);
  }
  return value;
}

// Local exact one-object member replacement over the exact parser: every patch
// member replaces its raw value in the document, absent members are preserved
// and unedited numeric lexemes survive byte-for-byte. The document never
// leaves the client for an edit.
export function replaceObjectMembers(document: string, patch: string): string {
  const members = exactObject(document, 'document');
  const edits = exactObject(patch, 'patch');
  return JSON.stringify({ ...members, ...edits });
}

// One exact-parsed object root; a numeric raw wrapper is not a JSON object.
function exactObject(text: string, what: string): Record<string, unknown> {
  const parsed = parseExactJSON(text);
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed) || JSON.isRawJSON(parsed)) {
    throw new Error(`${what} is not a JSON object`);
  }
  return parsed as Record<string, unknown>;
}

export function extractSettingsText(configurationText: string): string {
  const view = parseExactJSON(configurationText);
  if (view === null || typeof view !== 'object' || (view as { settings?: unknown }).settings === undefined) {
    throw new Error('configuration view carries no settings document');
  }
  return JSON.stringify((view as { settings: unknown }).settings);
}

export function parseSettingsObject(settingsText: string): JsonObject {
  return exactObject(settingsText, 'settings document') as JsonObject;
}

// One-member patch document; valueText is raw JSON text.
export function composeMemberPatch(member: string, valueText: string): string {
  return `{${JSON.stringify(member)}:${valueText}}`;
}

export function composeObjectPatch(members: Array<[string, string]>): string {
  return `{${members.map(([member, valueText]) => `${JSON.stringify(member)}:${valueText}`).join(',')}}`;
}

// Replace the whole `plugins` member; unedited documents keep exact string values.
export function composePluginsPatch(
  plugins: Record<string, string>,
  editedID: string,
  editedDocument: string | null,
): string {
  if (editedDocument === null) {
    const next = { ...plugins };
    delete next[editedID];
    return JSON.stringify(next);
  }
  // Spread and a computed property write own data properties, so a plugin named
  // `__proto__` stays ordinary data instead of hitting the prototype setter.
  return JSON.stringify({ ...plugins, [editedID]: editedDocument });
}

export function composeSessionsPatch(
  sessions: JsonObject,
  editedMember: string,
  editedText: string,
): string {
  const members: Array<[string, string]> = [];
  for (const [member, value] of Object.entries(sessions)) {
    members.push([member, member === editedMember ? editedText : JSON.stringify(value)]);
  }
  return composeObjectPatch(members);
}

// Exact Fork body; malformed replacement content is the invocation
// boundary's known local rejection before dispatch.
export function composeForkBody(boundaryItemID: string, operationID: string, contentText: string): string {
  return composeObjectPatch([
    ['boundary_item_id', JSON.stringify(boundaryItemID)],
    ['operation_id', JSON.stringify(operationID)],
    ['content', contentText],
  ]);
}
