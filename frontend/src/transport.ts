import { createGrpcWebTransport } from '@connectrpc/connect-web';

// One Envoy listener serves every service — Envoy demuxes on the gRPC path
// (/orders.OrderService/... vs /chat.v1.ChatService/...), so a single transport
// backs all clients. Transport-wide concerns (auth interceptors, retry policy,
// a baseUrl from import.meta.env) belong here.
export const transport = createGrpcWebTransport({
  baseUrl: 'http://localhost:8080',
});
