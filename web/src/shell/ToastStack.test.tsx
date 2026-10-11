import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import type { Root } from 'react-dom/client';
import type { StreamNotification } from '../stream';
import { ToastStack } from './ToastStack';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  vi.useFakeTimers();
  container = document.createElement('div');
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.useRealTimers();
});

const toast = (fields: Partial<StreamNotification> = {}): StreamNotification => ({
  id: 'n1',
  at: 0,
  kind: 'info',
  text: 'Saved',
  ...fields,
});

function render(notification: StreamNotification) {
  const onDismiss = vi.fn();
  act(() => root.render(<ToastStack notifications={[notification]} onDismiss={onDismiss} />));
  return onDismiss;
}

describe('ToastStack', () => {
  it('renders the title only when one is set', () => {
    render(toast());
    expect(container.querySelector('.toast-title')).toBeNull();
    expect(container.querySelector('div.toast-body')!.textContent).toBe('Saved');

    render(toast({ title: 'Draft' }));
    expect(container.querySelector('.toast-title')!.textContent).toBe('Draft');
    expect(container.querySelector('.toast-msg')!.textContent).toBe('Saved');
  });

  it('makes the body a button that runs onClick and then dismisses', () => {
    const onClick = vi.fn();
    const onDismiss = render(toast({ onClick }));
    const body = container.querySelector<HTMLButtonElement>('button.toast-body')!;

    act(() => body.click());

    expect(onClick).toHaveBeenCalledOnce();
    expect(onDismiss).toHaveBeenCalledExactlyOnceWith('n1');
    expect(onClick.mock.invocationCallOrder[0]!).toBeLessThan(
      onDismiss.mock.invocationCallOrder[0]!,
    );
  });

  it('dismisses on the per-kind timer by default', () => {
    const onDismiss = render(toast());
    act(() => {
      vi.advanceTimersByTime(4999);
    });
    expect(onDismiss).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(onDismiss).toHaveBeenCalledExactlyOnceWith('n1');
  });

  it('dismisses on durationMs when one is set', () => {
    const onDismiss = render(toast({ durationMs: 1000 }));
    act(() => {
      vi.advanceTimersByTime(999);
    });
    expect(onDismiss).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(onDismiss).toHaveBeenCalledExactlyOnceWith('n1');
  });
});
