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
  it('uses the nested declared message and preserves the failure payload', async () => {
    const body = '{"kind":"failure","error":{"code":"guest_code","message":"guest message","data":{"big":9007199254740993}},"partial_result":["partial",null,1]}';
    vi.mocked(Call).mockResolvedValue({ status: 422, body, invalid: false });
    const result = await invokeNative(() => client, 'extension', 'InvokeExtension', '{"body":{}}', JSON.parse);
    expect(result.kind).toBe('rejection');
    if (result.kind !== 'rejection') return;
    expect(result.message).toBe('guest message');
    expect(result.payload).toEqual(JSON.parse(body));
  });

  it('keeps the baseline Error message unchanged', async () => {
    vi.mocked(Call).mockResolvedValue({ status: 422, body: '{"code":"corrupt","message":"baseline"}', invalid: false });
    const result = await invokeNative(() => client, 'extension', 'InvokeExtension', '{"body":{}}', JSON.parse);
    expect(result.kind).toBe('rejection');
    if (result.kind !== 'rejection') return;
    expect(result.message).toBe('baseline');
  });
});
