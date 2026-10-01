import { createPromiseClient } from '@connectrpc/connect';
import { createGrpcWebTransport } from '@connectrpc/connect-web';
import { ChatService } from './gen/chat_connect';

// Every client shares this transport, pointed at Envoy. Transport-wide concerns
// (auth interceptors, retry policy, a baseUrl from import.meta.env) belong here.
const transport = createGrpcWebTransport({
  baseUrl: 'http://localhost:8080',
});

export const chatClient = createPromiseClient(ChatService, transport);
