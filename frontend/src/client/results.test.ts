import { describe, expect, it } from 'vitest';
import { decodeDeletionResult } from './results';

describe('Delete success decoder', () => {
  it.each([
    ['empty', '{"cleanup_failures":[]}', []],
    ['local', '{"cleanup_failures":[{"code":"storage","message":"remove tree: denied"}]}', [{ code: 'storage', message: 'remove tree: denied' }]],
    ['external', '{"cleanup_failures":[{"extension_id":"guest","code":"unavailable","message":"extension data cleanup is unavailable"}]}', [{ extension_id: 'guest', code: 'unavailable', message: 'extension data cleanup is unavailable' }]],
    ['empty message preserved', '{"cleanup_failures":[{"code":"storage","message":""}]}', [{ code: 'storage', message: '' }]],
  ])('accepts %s', (_name, body, want) => {
    expect(decodeDeletionResult(body).cleanup_failures).toEqual(want);
  });

  it.each([
    ['null root', 'null'],
    ['array root', '[]'],
    ['missing failures', '{}'],
    ['null failures', '{"cleanup_failures":null}'],
    ['wrong array', '{"cleanup_failures":{}}'],
    ['extra root member', '{"cleanup_failures":[],"extra":true}'],
    ['case alias root', '{"Cleanup_Failures":[]}'],
    ['non-object failure', '{"cleanup_failures":[null]}'],
    ['missing code', '{"cleanup_failures":[{"message":"x"}]}'],
    ['null code', '{"cleanup_failures":[{"code":null,"message":"x"}]}'],
    ['numeric code', '{"cleanup_failures":[{"code":1,"message":"x"}]}'],
    ['unknown code', '{"cleanup_failures":[{"code":"teapot","message":"x"}]}'],
    ['case code', '{"cleanup_failures":[{"code":"Storage","message":"x"}]}'],
    ['missing message', '{"cleanup_failures":[{"code":"storage"}]}'],
    ['numeric message', '{"cleanup_failures":[{"code":"storage","message":1}]}'],
    ['unknown member', '{"cleanup_failures":[{"code":"storage","message":"x","path":"/tmp"}]}'],
    ['case member', '{"cleanup_failures":[{"Code":"storage","Message":"x"}]}'],
    ['null owner', '{"cleanup_failures":[{"extension_id":null,"code":"unavailable","message":"x"}]}'],
    ['empty owner', '{"cleanup_failures":[{"extension_id":"","code":"unavailable","message":"x"}]}'],
    ['numeric owner', '{"cleanup_failures":[{"extension_id":5,"code":"unavailable","message":"x"}]}'],
    ['storage with owner', '{"cleanup_failures":[{"extension_id":"guest","code":"storage","message":"x"}]}'],
    ['unavailable without owner', '{"cleanup_failures":[{"code":"unavailable","message":"x"}]}'],
  ])('rejects %s', (_name, body) => {
    expect(() => decodeDeletionResult(body)).toThrow();
  });
});
