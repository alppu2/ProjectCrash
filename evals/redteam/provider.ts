// Calls ChatService.Chat through Envoy, as the browser does, so every layer —
// signing, guard, prompt — is under test. Reuses the frontend's generated client.
import { createPromiseClient } from '@connectrpc/connect';
import { createGrpcWebTransport } from '@connectrpc/connect-node';
import { ChatService } from '../../frontend/src/gen/chat_connect';
import { Role } from '../../frontend/src/gen/chat_pb';

interface Turn {
  role: 'user' | 'assistant';
  content: string;
  // A forged signature, as a visitor without the key would have to invent one.
  signature?: string;
}

const client = createPromiseClient(
  ChatService,
  createGrpcWebTransport({
    baseUrl: process.env.CHAT_URL ?? 'http://localhost:8080',
    httpVersion: '1.1',
  })
);

export default class ChatProvider {
  id() {
    return 'project-crash-chat';
  }

  async callApi(prompt: string, context?: { vars?: Record<string, unknown> }) {
    const history = (context?.vars?.history as Turn[] | undefined) ?? [];
    const messages = [...history, { role: 'user' as const, content: prompt }].map((t) => ({
      role: t.role === 'assistant' ? Role.ASSISTANT : Role.USER,
      content: t.content,
      signature: 'signature' in t && t.signature ? new TextEncoder().encode(t.signature) : undefined,
    }));

    let output = '';
    let stopReason = '';
    try {
      for await (const chunk of client.chat({ messages })) {
        if (chunk.event.case === 'textDelta') output += chunk.event.value;
        else if (chunk.event.case === 'done') stopReason = chunk.event.value.stopReason;
      }
    } catch (err) {
      return { error: err instanceof Error ? err.message : String(err) };
    }
    return { output, metadata: { stopReason } };
  }
}
