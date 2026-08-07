import { createPromiseClient } from '@connectrpc/connect';
import { transport } from './transport';
import { ChatService } from './gen/chat_connect';

export const chatClient = createPromiseClient(ChatService, transport);
