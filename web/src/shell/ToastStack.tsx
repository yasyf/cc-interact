// The floating toast stack: the most recent stream toasts as a fixed top-right
// column, each auto-dismissed on a per-kind budget. Errors linger longest and get
// role=alert; the rest are role=status. Decoupled from any domain — the consumer
// wires it from useEventStream().

import { useEffect, useRef, useState } from 'react';
import type { StreamNotification, ToastKind } from '../stream';

const MAX_VISIBLE = 4;

const AUTO_DISMISS_MS: Record<ToastKind, number> = {
  info: 5000,
  warn: 8000,
  error: 10000,
};

export interface ToastStackProps {
  notifications: StreamNotification[];
  onDismiss: (id: string) => void;
}

export function ToastStack({ notifications, onDismiss }: ToastStackProps) {
  const visible = notifications.slice(-MAX_VISIBLE).reverse();
  // Hover pauses the stack so a toast can be read: enter clears every timer, leave
  // restarts them from full duration.
  const [paused, setPaused] = useState(false);
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  // Timers survive re-renders, so they must dismiss through a ref — a timer
  // scheduled before an onDismiss identity change would otherwise fire the stale
  // callback.
  const dismiss = useRef(onDismiss);
  useEffect(() => {
    dismiss.current = onDismiss;
  }, [onDismiss]);

  // One effect owns every dismiss timer. Paused → clear all. Otherwise reconcile:
  // drop timers for toasts that scrolled out of the visible window and schedule a
  // fresh full-duration timer for each newly visible one, leaving running timers be.
  useEffect(() => {
    const active = timers.current;
    if (paused) {
      active.forEach(clearTimeout);
      active.clear();
      return;
    }
    const shown = notifications.slice(-MAX_VISIBLE);
    const shownIds = new Set(shown.map((n) => n.id));
    for (const [id, timer] of active) {
      if (!shownIds.has(id)) {
        clearTimeout(timer);
        active.delete(id);
      }
    }
    for (const n of shown) {
      if (!active.has(n.id)) {
        active.set(
          n.id,
          setTimeout(() => dismiss.current(n.id), AUTO_DISMISS_MS[n.kind]),
        );
      }
    }
  }, [notifications, paused]);

  useEffect(
    () => () => {
      timers.current.forEach(clearTimeout);
      timers.current.clear();
    },
    [],
  );

  if (visible.length === 0) return null;

  return (
    <div
      className="toast-stack"
      onMouseEnter={() => setPaused(true)}
      onMouseLeave={() => setPaused(false)}
    >
      {visible.map((n) => (
        <div
          key={n.id}
          className={`toast toast-${n.kind}`}
          role={n.kind === 'error' ? 'alert' : 'status'}
        >
          <span className="toast-msg">{n.text}</span>
          <button
            type="button"
            className="toast-x"
            aria-label="dismiss"
            onClick={() => onDismiss(n.id)}
          >
            ×
          </button>
        </div>
      ))}
    </div>
  );
}
