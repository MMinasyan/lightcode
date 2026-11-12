// The native rejection text understands an explicitly declared extension
// failure without sniffing restore-shaped members; the raw body still reaches
// the consumer as the rejection payload.
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../../client-bindings/wailsjs/go/desktopclient/Host', () => ({
  Call: vi.fn(),
}));

import { Call } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import { invokeNative } from './results';

const client = { generation: '1' };

beforeEach(() => {
  vi.clearAllMocks();
});

describe('invokeNative declared extension failures', () => {
  it('uses the nested declared message, exact body and exact numeric tokens', async () => {
    const body = '{\n  "kind": "failure",\n  "error": {"code":"guest_code","message":"guest message","data":{"big":9007199254740993,"text":"line\\nbreak\\t<>&"}},\n  "partial_result": ["partial", null, 9007199254740993]\n}';
    vi.mocked(Call).mockResolvedValue({ status: 422, body, invalid: false });
    const result = await invokeNative(() => client, 'extension', 'InvokeExtension', '{"body":{}}', JSON.parse);
    expect(result.kind).toBe('rejection');
    if (result.kind !== 'rejection') return;
    expect(result.message).toBe('guest message');
    expect(result.body).toBe(body);
    expect(JSON.stringify(result.payload)).toContain('9007199254740993');
    expect(JSON.stringify(result.payload)).toContain('line\\nbreak\\t<>&');
  });

  it('keeps the baseline Error message, body and partial numeric tokens', async () => {
    const body = '{\n  "code": "corrupt",\n  "message": "baseline",\n  "partial_result": {"big": 9007199254740993, "text": "line\\nbreak"}\n}';
    vi.mocked(Call).mockResolvedValue({ status: 500, body, invalid: false });
    const result = await invokeNative(() => client, 'extension', 'InvokeExtension', '{"body":{}}', JSON.parse);
    expect(result.kind).toBe('rejection');
    if (result.kind !== 'rejection') return;
    expect(result.message).toBe('baseline');
    expect(result.body).toBe(body);
    expect(JSON.stringify(result.payload)).toContain('9007199254740993');
    expect(JSON.stringify(result.payload)).toContain('line\\nbreak');
  });

  it('keeps local syntax refusals without an HTTP body', async () => {
    const result = await invokeNative(() => client, 'extension', 'InvokeExtension', '{bad', JSON.parse);
    expect(result.kind).toBe('rejection');
    if (result.kind !== 'rejection') return;
    expect(result.body).toBeUndefined();
  });
});
