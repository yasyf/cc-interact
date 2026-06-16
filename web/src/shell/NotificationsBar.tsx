// The notifications strip: the connection pill plus the most recent toasts.
// Decoupled from any domain — the consumer wires it from useEventStream().

import type { StreamNotification } from '../stream';
import { ConnectionFrame } from './ConnectionFrame';

export interface NotificationsBarProps {
  connected: boolean;
  notifications: StreamNotification[];
  onDismiss: (id: string) => void;
}

export function NotificationsBar({ connected, notifications, onDismiss }: NotificationsBarProps) {
  const recent = notifications.slice(-5).reverse();

  return (
    <aside className="notifications">
      <ConnectionFrame connected={connected} />
      <div className="notif-list">
        {recent.map((n) => (
          <div key={n.id} className={`notif notif-${n.kind}`}>
            <span className="notif-msg">{n.text}</span>
            <button
              type="button"
              className="notif-x"
              aria-label="dismiss"
              onClick={() => onDismiss(n.id)}
            >
              ×
            </button>
          </div>
        ))}
      </div>
    </aside>
  );
}
