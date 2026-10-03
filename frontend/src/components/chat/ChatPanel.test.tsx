import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import ChatPanel from './ChatPanel';
import { ChatChunk, Role, WarmupResponse } from '../../gen/chat_pb';
import { chatClient } from '../../api';

vi.mock('../../api', () => ({
  chatClient: { chat: vi.fn(), warmup: vi.fn() },
}));

const mockChat = vi.mocked(chatClient.chat);
const mockWarmup = vi.mocked(chatClient.warmup);

// No jest-dom in this repo; plain DOM properties instead of its matchers.
function textbox(): HTMLInputElement {
  return screen.getByRole('textbox') as HTMLInputElement;
}

describe('ChatPanel', () => {
  // RTL only auto-cleans with vitest globals, which this repo leaves off.
  afterEach(cleanup);

  beforeEach(() => {
    mockChat.mockReset();
    mockWarmup.mockReset();
  });

  // A message typed during a cold load would pay the load anyway and race
  // the welcome into the list.
  it('keeps the input disabled until the assistant is warm', async () => {
    let release!: () => void;
    mockWarmup.mockReturnValue(
      new Promise<WarmupResponse>((resolve) => {
        release = () => resolve(new WarmupResponse());
      })
    );

    render(<ChatPanel />);

    expect(textbox().disabled).toBe(true);
    expect(screen.getByText('Assistant is warming up…')).toBeTruthy();

    release();

    await waitFor(() => expect(textbox().disabled).toBe(false));
    expect(await screen.findByText(/^Hi,/)).toBeTruthy();
  });

  // The welcome is page copy. If it leaked into history the model would
  // treat it as its own prior turn.
  it('sends only the user turn after the welcome', async () => {
    mockWarmup.mockResolvedValue(new WarmupResponse());
    mockChat.mockImplementation(() =>
      (async function* () {
        yield new ChatChunk({ event: { case: 'textDelta', value: 'hello' } });
      })()
    );

    render(<ChatPanel />);
    await screen.findByText(/^Hi,/);

    fireEvent.change(textbox(), { target: { value: 'who is he?' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(mockChat).toHaveBeenCalledTimes(1));
    expect(mockChat.mock.calls[0][0].messages).toEqual([
      { role: Role.USER, content: 'who is he?' },
    ]);
  });

  // Retrieval and condensing run before the first token, so a turn can sit
  // silent for seconds; without the indicator it reads as a dead send.
  it('shows the assistant typing until the first text arrives', async () => {
    mockWarmup.mockResolvedValue(new WarmupResponse());
    let release!: () => void;
    const firstToken = new Promise<void>((resolve) => (release = resolve));
    mockChat.mockImplementation(() =>
      (async function* () {
        await firstToken;
        yield new ChatChunk({ event: { case: 'textDelta', value: 'hello' } });
      })()
    );

    render(<ChatPanel />);
    await screen.findByText(/^Hi,/);
    fireEvent.change(textbox(), { target: { value: 'who is he?' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    expect((await screen.findByRole('status')).textContent).toBe(
      'Assistant is typing'
    );

    release();

    expect(await screen.findByText('hello')).toBeTruthy();
    expect(screen.queryByRole('status')).toBeNull();
  });

  // The scrollbar is drawn only while this attribute is set; if it stuck, the
  // bar would never hide again, and if it never set, it would never show.
  it('marks the list as scrolling until scrolling stops', () => {
    vi.useFakeTimers();
    try {
      mockWarmup.mockReturnValue(new Promise<WarmupResponse>(() => {}));
      render(<ChatPanel />);
      const list = screen.getByRole('list');

      fireEvent.scroll(list);
      expect(list.hasAttribute('data-scrolling')).toBe(true);

      vi.advanceTimersByTime(500);
      fireEvent.scroll(list);
      vi.advanceTimersByTime(900);
      expect(list.hasAttribute('data-scrolling')).toBe(true);

      vi.advanceTimersByTime(200);
      expect(list.hasAttribute('data-scrolling')).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('offers a retry when the assistant is unavailable', async () => {
    mockWarmup.mockRejectedValueOnce(new Error('the assistant is unavailable'));
    mockWarmup.mockResolvedValue(new WarmupResponse());

    render(<ChatPanel />);

    fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));

    await waitFor(() => expect(textbox().disabled).toBe(false));
    expect(mockWarmup).toHaveBeenCalledTimes(2);
  });

  // Retry unmounts the focused button; without this a keyboard user is
  // dropped to the page body when the chat comes back.
  it('returns focus to the input once a retry succeeds', async () => {
    mockWarmup.mockRejectedValueOnce(new Error('the assistant is unavailable'));
    mockWarmup.mockResolvedValue(new WarmupResponse());

    render(<ChatPanel />);
    const retry = await screen.findByRole('button', { name: 'Retry' });
    retry.focus();
    fireEvent.click(retry);

    await waitFor(() => expect(document.activeElement).toBe(textbox()));
  });

  // Screen-reader users cannot see the chat become usable or go offline.
  it('announces warming as a status and offline as an alert', async () => {
    mockWarmup.mockRejectedValue(new Error('the assistant is unavailable'));

    render(<ChatPanel />);

    expect(screen.getByRole('status').textContent).toBe(
      'Assistant is warming up…'
    );
    expect((await screen.findByRole('alert')).textContent).toContain('offline');
  });
});
