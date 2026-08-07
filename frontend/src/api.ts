import { createPromiseClient } from '@connectrpc/connect';
import { createGrpcWebTransport } from '@connectrpc/connect-web';
import { OrderService } from './gen/service_connect';
import { ChatService } from './gen/chat_connect';

// One Envoy listener serves every service — it demuxes on the gRPC path
// (/orders.OrderService/... vs /chat.v1.ChatService/...), so a single transport
// backs all clients. Transport-wide concerns (auth interceptors, retry policy,
// a baseUrl from import.meta.env) belong here.
const transport = createGrpcWebTransport({
  baseUrl: 'http://localhost:8080',
});

// One client per service: createPromiseClient binds exactly one service
// descriptor, and the method types come from it.
export const client = createPromiseClient(OrderService, transport);
export const chatClient = createPromiseClient(ChatService, transport);
