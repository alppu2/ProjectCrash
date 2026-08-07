import { createPromiseClient } from '@connectrpc/connect';
import { transport } from './transport';
import { OrderService } from './gen/service_connect';

export const client = createPromiseClient(OrderService, transport);
