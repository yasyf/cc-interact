import { afterEach, describe, expect, it } from 'vitest';
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import type { Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { applyCacheStep, useOptimisticMutation } from './index';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

interface Board {
  items: string[];
}

interface Call {
  resolve: () => void;
  reject: (err: Error) => void;
}

const key = ['board', 'b1'];
const add = (item: string) => (board: Board): Board => ({ items: [...board.items, item] });
const tick = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
const flush = () =>
  act(async () => {
    await tick();
    await tick();
  });

let root: Root;

afterEach(() => {
  act(() => root.unmount());
});

function mount(onError?: (board: Board) => void) {
  const qc = new QueryClient();
  qc.setQueryData<Board>(key, { items: ['seed'] });
  const calls: Call[] = [];
  let mutate!: (item: string) => void;

  function Probe() {
    mutate = useOptimisticMutation<string, void, Board>({
      mutationFn: () => new Promise<void>((resolve, reject) => calls.push({ resolve, reject })),
      queryKey: () => key,
      applyOptimistic: (board, item) => add(item)(board),
      onError: () => onError?.(qc.getQueryData<Board>(key)!),
      scope: 'b1',
    }).mutate;
    return null;
  }

  root = createRoot(document.createElement('div'));
  act(() =>
    root.render(
      <QueryClientProvider client={qc}>
        <Probe />
      </QueryClientProvider>,
    ),
  );

  return {
    qc,
    calls,
    items: () => qc.getQueryData<Board>(key)!.items,
    mutate: async (item: string) => {
      mutate(item);
      await flush();
    },
    resolve: async (call: number) => {
      calls[call]!.resolve();
      await flush();
    },
    reject: async (call: number) => {
      calls[call]!.reject(new Error('post failed'));
      await flush();
    },
  };
}

describe('useOptimisticMutation', () => {
  it('restores the pre-mutation cache on error', async () => {
    const board = mount();
    await board.mutate('a');
    expect(board.items()).toEqual(['seed', 'a']);
    await board.reject(0);
    expect(board.items()).toEqual(['seed']);
  });

  it('keeps a step applied between mutate and error', async () => {
    const board = mount();
    await board.mutate('a');
    applyCacheStep(board.qc, key, add('frame'));
    expect(board.items()).toEqual(['seed', 'a', 'frame']);
    await board.reject(0);
    expect(board.items()).toEqual(['seed', 'frame']);
  });

  it('patches at mutate time while the scope holds the second post back', async () => {
    const board = mount();
    await board.mutate('a');
    await board.mutate('b');
    expect(board.items()).toEqual(['seed', 'a', 'b']);
    expect(board.calls).toHaveLength(1);
  });

  it('leaves the later patch when the earlier mutation fails and the later succeeds', async () => {
    const board = mount();
    await board.mutate('a');
    await board.mutate('b');
    await board.reject(0);
    expect(board.items()).toEqual(['seed', 'b']);
    await board.resolve(1);
    expect(board.items()).toEqual(['seed', 'b']);
  });

  it('returns to the base when both mutations fail', async () => {
    const board = mount();
    await board.mutate('a');
    await board.mutate('b');
    await board.reject(0);
    await board.reject(1);
    expect(board.items()).toEqual(['seed']);
  });

  it('leaves the earlier patch when the earlier mutation succeeds and the later fails', async () => {
    const board = mount();
    await board.mutate('a');
    await board.mutate('b');
    await board.resolve(0);
    expect(board.items()).toEqual(['seed', 'a', 'b']);
    await board.reject(1);
    expect(board.items()).toEqual(['seed', 'a']);
  });

  it('calls onError with the cache already rolled back', async () => {
    const seen: string[][] = [];
    const board = mount(({ items }) => seen.push(items));
    await board.mutate('a');
    await board.reject(0);
    expect(seen).toEqual([['seed']]);
  });

  it('drops the journal once every mutation settles', async () => {
    const board = mount();
    await board.mutate('a');
    await board.reject(0);
    board.qc.setQueryData<Board>(key, { items: ['refetched'] });
    applyCacheStep(board.qc, key, add('frame'));
    expect(board.items()).toEqual(['refetched', 'frame']);
    await board.mutate('c');
    await board.reject(1);
    expect(board.items()).toEqual(['refetched', 'frame']);
  });
});
