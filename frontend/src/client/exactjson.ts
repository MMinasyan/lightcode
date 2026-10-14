// Exact-JSON text helpers: parse source access plus JSON.rawJSON keep every
// numeric lexeme exactly as carried. Native PatchJSON owns document mutation.
import type { JsonObject, SnapshotFile } from '../generated/protocol/types.gen';

// Parse one document, replacing every number with its exact source token.
export function parseExactJSON(text: string): unknown {
  return JSON.parse(text, (_key, value, context) => {
    if (typeof value === 'number' && context !== undefined && typeof context.source === 'string') {
      return JSON.rawJSON(context.source);
    }
    return value;
  });
}

export function rawJSONText(value: unknown): string | null {
  if (value === null || typeof value !== 'object') {
    return null;
  }
  const candidate = value as { rawJSON?: unknown };
  return typeof candidate.rawJSON === 'string' ? candidate.rawJSON : null;
}

export function extractSettingsText(configurationText: string): string {
  const view = parseExactJSON(configurationText);
  if (view === null || typeof view !== 'object' || (view as { settings?: unknown }).settings === undefined) {
    throw new Error('configuration view carries no settings document');
  }
  return JSON.stringify((view as { settings: unknown }).settings);
}

export function parseSettingsObject(settingsText: string): JsonObject {
  const parsed = parseExactJSON(settingsText);
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error('settings document is not a JSON object');
  }
  return parsed as JsonObject;
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

// One retained turn: exact numeric boundary token plus the generated file
// objects returned by reference.
export interface RetainedTurnText {
  turnText: string;
  files: SnapshotFile[];
}

export function readRetainedTurnsText(document: unknown): RetainedTurnText[] {
  if (document === null || typeof document !== 'object') {
    throw new Error('retained snapshot document is not a JSON object');
  }
  const turns = (document as { turns?: unknown }).turns;
  if (!Array.isArray(turns)) {
    throw new Error('retained snapshot document carries no turns array');
  }
  return turns.map((entry) => {
    if (entry === null || typeof entry !== 'object') {
      throw new Error('retained turn is not a JSON object');
    }
    const turn = (entry as { turn?: unknown }).turn;
    // The accepted number/raw-token rule, then native JSON.stringify derives the
    // exact boundary text (JSON.rawJSON included).
    if (typeof turn !== 'number' && rawJSONText(turn) === null) {
      throw new Error('retained turn boundary is not a JSON number');
    }
    const turnText = JSON.stringify(turn);
    if (turnText === undefined) {
      throw new Error('retained turn boundary is not a JSON number');
    }
    const files = (entry as { files?: unknown }).files;
    if (!Array.isArray(files)) {
      throw new Error('retained turn carries no files array');
    }
    // Validate the consumed shape in place; the original array and file objects
    // are returned by reference, never rebuilt.
    for (const file of files) {
      if (file === null || typeof file !== 'object') {
        throw new Error('retained snapshot file is not a JSON object');
      }
      const candidate = file as { path?: unknown; existed?: unknown };
      if (typeof candidate.path !== 'string' || typeof candidate.existed !== 'boolean') {
        throw new Error('retained snapshot file is malformed');
      }
    }
    return { turnText, files: files as SnapshotFile[] };
  });
}
