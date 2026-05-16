import { createPromiseClient } from '@connectrpc/connect';
import { createGrpcWebTransport } from '@connectrpc/connect-web';
import { OrderService } from './gen/service_connect';

const transport = createGrpcWebTransport({
  baseUrl: 'http://localhost:8080',
});

// The client is what we use in our components
export const client = createPromiseClient(OrderService, transport);
