import { Code, ConnectError } from '@connectrpc/connect';
import { proto3, ScalarType } from '@bufbuild/protobuf';

// google.rpc.ErrorInfo, declared by hand: one message is not worth a
// googleapis dependency in buf.gen.yaml.
export const ErrorInfo = proto3.makeMessageType('google.rpc.ErrorInfo', () => [
  { no: 1, name: 'reason', kind: 'scalar', T: ScalarType.STRING },
  { no: 2, name: 'domain', kind: 'scalar', T: ScalarType.STRING },
  {
    no: 3,
    name: 'metadata',
    kind: 'map',
    K: ScalarType.STRING,
    V: { kind: 'scalar', T: ScalarType.STRING },
  },
]);

const clock = new Intl.DateTimeFormat(undefined, {
  hour: '2-digit',
  minute: '2-digit',
});

// Copy keyed on chat-service's refusal reasons; read by people evaluating the
// work, so it is reviewed like the guard's refusal.
export function describeError(err: unknown): string {
  if (err instanceof ConnectError && err.code === Code.DeadlineExceeded) {
    return 'That took too long. Please try again.';
  }
  if (!(err instanceof ConnectError) || err.code !== Code.ResourceExhausted) {
    return err instanceof Error ? err.message : 'Unknown error';
  }
  const info = err.findDetails(ErrorInfo)[0];
  const resetsAt = (info?.metadata as Record<string, string> | undefined)
    ?.resets_at;
  const at = resetsAt ? clock.format(new Date(resetsAt)) : 'midnight UTC';
  switch (info?.reason) {
    case 'TOO_MANY_STREAMS':
      return 'Finish your other conversation first.';
    case 'BUDGET_EXHAUSTED':
      return `You've reached today's limit. It resets at ${at}.`;
    case 'GLOBAL_BUDGET_EXHAUSTED':
      return `The assistant is resting for today. Back at ${at}.`;
    default:
      // Envoy's per-IP limit, which answers before chat-service.
      return 'Slow down a moment and try again.';
  }
}
