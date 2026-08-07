import { createPromiseClient } from '@connectrpc/connect';
import { createGrpcWebTransport } from '@connectrpc/connect-web';
import { ChatService } from './gen/chat_connect';

// Same Envoy listener as the packet client — Envoy demuxes on the gRPC path,
// so no second port is involved.
const transport = createGrpcWebTransport({
  baseUrl: 'http://localhost:8080',
});

export const chatClient = createPromiseClient(ChatService, transport);
