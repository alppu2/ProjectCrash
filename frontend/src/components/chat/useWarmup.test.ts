import { StrictMode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook, waitFor } from '@testing-library/react';
import useWarmup from './useWarmup';
import { WELCOME_MESSAGE } from './welcome';
import { WarmupResponse } from '../../gen/chat_pb';
import { chatClient } from '../../api';

vi.mock('../../api', () => ({
  chatClient: { warmup: vi.fn() },
}));

const mockWarmup = vi.mocked(chatClient.warmup);

describe('useWarmup', () => {
  beforeEach(() => {
    mockWarmup.mockReset();
  });

  it('reveals the welcome once the assistant is warm', async () => {
    mockWarmup.mockResolvedValue(new WarmupResponse());

    const { result } = renderHook(() => useWarmup(0));

    expect(result.current.status).toBe('warming');
    expect(result.current.welcome).toBe('');
    await waitFor(() => expect(result.current.welcome).toBe(WELCOME_MESSAGE));
    expect(result.current.status).toBe('ready');
  });

  it('reports unavailable without a welcome when warmup fails', async () => {
    mockWarmup.mockRejectedValue(new Error('the assistant is unavailable'));

    const { result } = renderHook(() => useWarmup(0));

    await waitFor(() => expect(result.current.status).toBe('unavailable'));
    expect(result.current.welcome).toBe('');
  });

  it('warms again on retry', async () => {
    mockWarmup.mockRejectedValueOnce(new Error('the assistant is unavailable'));
    mockWarmup.mockResolvedValue(new WarmupResponse());

    const { result } = renderHook(() => useWarmup(0));
    await waitFor(() => expect(result.current.status).toBe('unavailable'));

    act(() => result.current.retry());

    expect(result.current.status).toBe('warming');
    await waitFor(() => expect(result.current.status).toBe('ready'));
    expect(mockWarmup).toHaveBeenCalledTimes(2);
  });

  // StrictMode mounts, aborts and remounts. A real transport rejects the
  // aborted call late; if that flips status, every dev load shows "offline".
  it('ignores a late rejection from the aborted first mount', async () => {
    mockWarmup.mockImplementationOnce(
      (_req, opts) =>
        new Promise<WarmupResponse>((_, reject) => {
          opts?.signal?.addEventListener('abort', () =>
            setTimeout(() => reject(new Error('aborted')), 20)
          );
        })
    );
    mockWarmup.mockResolvedValue(new WarmupResponse());

    const { result } = renderHook(() => useWarmup(0), { wrapper: StrictMode });

    await waitFor(() => expect(result.current.status).toBe('ready'));
    await new Promise((resolve) => setTimeout(resolve, 40));
    expect(result.current.status).toBe('ready');
  });
});
