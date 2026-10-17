import { describe, expect, it } from 'vitest';
import { Code, ConnectError } from '@connectrpc/connect';
import { describeError, ErrorInfo } from './refusal';

// As the transport decodes it: an incoming detail, type name plus bytes.
function refused(reason: string, metadata: Record<string, string> = {}) {
  const info = new ErrorInfo({ reason, domain: 'chat.v1', metadata });
  const err = new ConnectError('turn refused', Code.ResourceExhausted);
  err.details.push({ type: 'google.rpc.ErrorInfo', value: info.toBinary() });
  return err;
}

describe('describeError', () => {
  it('tells a spent visitor when the budget resets', () => {
    const msg = describeError(
      refused('BUDGET_EXHAUSTED', { resets_at: '2026-10-12T00:00:00Z' })
    );
    expect(msg).toMatch(/^You've reached today's limit\. It resets at .+\.$/);
  });

  it('words a spent site differently from a spent visitor', () => {
    const msg = describeError(
      refused('GLOBAL_BUDGET_EXHAUSTED', { resets_at: '2026-10-12T00:00:00Z' })
    );
    expect(msg).toMatch(/^The assistant is resting for today\. Back at .+\.$/);
  });

  it('asks a visitor with too many open streams to finish one', () => {
    expect(describeError(refused('TOO_MANY_STREAMS'))).toBe(
      'Finish your other conversation first.'
    );
  });

  // Envoy's rate limit answers before chat-service and carries no ErrorInfo.
  it('reads ResourceExhausted without a reason as the rate limit', () => {
    const err = new ConnectError('rate limited', Code.ResourceExhausted);
    expect(describeError(err)).toBe('Slow down a moment and try again.');
  });

  // chat-service's turn deadline; the raw message would read "[deadline_exceeded] ...".
  it('words a turn that hit its deadline', () => {
    const err = new ConnectError(
      'the reply took too long',
      Code.DeadlineExceeded
    );
    expect(describeError(err)).toBe('That took too long. Please try again.');
  });

  it('passes other errors through unchanged', () => {
    expect(describeError(new Error('boom'))).toBe('boom');
    expect(describeError('nope')).toBe('Unknown error');
  });
});
