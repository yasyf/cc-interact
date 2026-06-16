// The realtime transport: one EventSource per subject that patches a TanStack
// Query cache. Every domain-specific decision — which key to patch, how to
// reduce a frame, what to toast, when a frame applies, presence — is supplied
// by the consumer through the config callbacks; this module owns only the
// EventSource lifecycle and the cache-patch / replay-gate / presence
// choreography.

import { createContext, useContext, useEffect, useState } from 'react';
import type { FC, ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { QueryClient } from '@tanstack/react-query';

export interface StreamToast {
  kind: string;
  text: string;
}

export interface StreamNotification extends StreamToast {
  id: string;
  at: number;
}

export interface EventStreamValue {
  // Whether the EventSource is currently open.
  connected: boolean;
  // The peer on the other end of the channel (e.g. the agent). null until the
  // first presence frame arrives; toggled by config.peerPresence.
  peerPresent: boolean | null;
  notifications: StreamNotification[];
  dismiss(id: string): void;
}

export interface EventContext<Cache, Subject, Scope> {
  queryClient: QueryClient;
  // The cache value read before this frame's reduce — undefined when the query
  // has not loaded yet.
  cache: Cache | undefined;
  subject: Subject;
  scope: Scope;
}

export interface EventStreamConfig<
  Ev extends { type: string },
  Cache,
  Subject = string,
  Scope = void,
> {
  // The query key this subject's cache lives under.
  queryKey: (subject: Subject, scope: Scope) => readonly unknown[];
  // Consumer-supplied domain reduction of one frame into the cache.
  reduce: (cache: Cache, ev: Ev) => Cache;
  // The /events URL; defaults to `/events?session=<subject>` matching the Go
  // SSE plane (resume rides the Last-Event-ID header the browser sends).
  url?: (subject: Subject, scope: Scope) => string;
  // Optional notification mapping; a null return suppresses the toast.
  toast?: (ev: Ev) => StreamToast | null;
  // Whether a frame applies to the cache on screen (e.g. a version filter).
  // Defaults to always-true.
  appliesTo?: (ev: Ev, cache: Cache) => boolean;
  // The replay gate: a frame whose lastEventId is at or below this is a
  // historical replay and patches state without toasting. Omit to toast every
  // frame.
  highWaterSeq?: (cache: Cache) => number;
  // Presence projection for a frame: true/false to set the peer dot, null when
  // the frame is not a presence event.
  peerPresence?: (ev: Ev) => boolean | null;
  // Escape hatch for cross-key invalidation and external side-state.
  onEvent?: (ev: Ev, ctx: EventContext<Cache, Subject, Scope>) => void;
}

export interface EventStreamProviderProps<Subject, Scope> {
  subject: Subject;
  scope?: Scope;
  children: ReactNode;
}

export interface EventStream<Subject, Scope> {
  EventStreamProvider: FC<EventStreamProviderProps<Subject, Scope>>;
  useEventStream: () => EventStreamValue;
}

const defaultUrl = (subject: unknown): string =>
  `/events?session=${encodeURIComponent(String(subject))}`;

let notificationSeq = 0;

export function createEventStream<
  Ev extends { type: string },
  Cache,
  Subject = string,
  Scope = void,
>(config: EventStreamConfig<Ev, Cache, Subject, Scope>): EventStream<Subject, Scope> {
  const Context = createContext<EventStreamValue | null>(null);

  function useEventStream(): EventStreamValue {
    const value = useContext(Context);
    if (!value) throw new Error('useEventStream must be used within EventStreamProvider');
    return value;
  }

  const EventStreamProvider: FC<EventStreamProviderProps<Subject, Scope>> = ({
    subject,
    scope,
    children,
  }) => {
    const queryClient = useQueryClient();
    const [connected, setConnected] = useState(false);
    const [peerPresent, setPeerPresent] = useState<boolean | null>(null);
    const [notifications, setNotifications] = useState<StreamNotification[]>([]);

    useEffect(() => {
      // An absent scope is the Scope=void / default case; the consumer's
      // queryKey and url callbacks own how they treat it.
      const resolvedScope = scope as Scope;
      const url = config.url
        ? config.url(subject, resolvedScope)
        : defaultUrl(subject);
      const source = new EventSource(url);

      source.onopen = () => setConnected(true);
      source.onerror = () => setConnected(false);

      // The Go plane emits no `event:` field, so every frame lands on onmessage
      // and the discriminant lives inside the JSON payload.
      source.onmessage = (raw: MessageEvent<string>) => {
        const ev = JSON.parse(raw.data) as Ev;
        const key = config.queryKey(subject, resolvedScope);
        const current = queryClient.getQueryData<Cache>(key);

        if (current !== undefined && (config.appliesTo ? config.appliesTo(ev, current) : true)) {
          queryClient.setQueryData<Cache>(key, config.reduce(current, ev));
        }

        if (config.peerPresence) {
          const present = config.peerPresence(ev);
          if (present !== null) setPeerPresent(present);
        }

        config.onEvent?.(ev, { queryClient, cache: current, subject, scope: resolvedScope });

        // The stream replays the whole log from cursor 0 on every load; replayed
        // frames (lastEventId at or below the snapshot's high-water seq) patch
        // state above but never toast.
        const live =
          config.highWaterSeq === undefined
            ? true
            : current !== undefined && Number(raw.lastEventId) > config.highWaterSeq(current);
        const toast = live && config.toast ? config.toast(ev) : null;
        if (toast) {
          const entry: StreamNotification = { ...toast, id: `n${++notificationSeq}`, at: Date.now() };
          setNotifications((prev) => [...prev, entry].slice(-50));
        }
      };

      return () => source.close();
    }, [queryClient, subject, scope]);

    function dismiss(id: string) {
      setNotifications((prev) => prev.filter((n) => n.id !== id));
    }

    return (
      <Context.Provider value={{ connected, peerPresent, notifications, dismiss }}>
        {children}
      </Context.Provider>
    );
  };

  return { EventStreamProvider, useEventStream };
}
