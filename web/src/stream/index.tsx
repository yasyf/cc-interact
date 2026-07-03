// The realtime transport: one EventSource per subject that patches a TanStack
// Query cache. Every domain-specific decision — which key to patch, how to
// reduce a frame, what to toast, when a frame applies, presence — is supplied
// by the consumer through the config callbacks; this module owns only the
// EventSource lifecycle and the cache-patch / replay-gate / presence
// choreography.

import { createContext, useContext, useEffect, useRef, useState } from 'react';
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
  // The replay-gate fallback for servers that predate the caught-up marker: a
  // frame whose lastEventId is at or below this is a historical replay and patches
  // state without toasting. The caught-up marker, when present, supersedes it.
  // Omit both to toast every frame.
  highWaterSeq?: (cache: Cache) => number;
  // Fires once per connection when the server's caught-up marker arrives, carrying
  // the high-water seq of the replay just completed; frames after it are the live
  // tail. Servers that predate the marker never call it.
  onCaughtUp?: (seq: number) => void;
  // Presence projection for a frame: true/false to set the peer dot, null when
  // the frame is not a presence event.
  peerPresence?: (ev: Ev) => boolean | null;
  // Seeds peerPresent from the snapshot the first time the cache loads, before
  // any presence frame — so a consumer that derives the initial peer state from
  // its GET snapshot renders it without flickering through null. A presence frame
  // that lands first wins.
  initialPeerPresence?: (cache: Cache) => boolean | null;
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
    // The high-water seq of the last replay, set by the caught-up marker; null
    // until the marker arrives (or on a server that predates it).
    const caughtUpSeq = useRef<number | null>(null);

    useEffect(() => {
      // An absent scope is the Scope=void / default case; the consumer's
      // queryKey and url callbacks own how they treat it.
      const resolvedScope = scope as Scope;
      const url = config.url
        ? config.url(subject, resolvedScope)
        : defaultUrl(subject);
      const source = new EventSource(url);
      caughtUpSeq.current = null;

      source.onopen = () => setConnected(true);
      source.onerror = () => setConnected(false);

      // A named SSE event never reaches onmessage; it records the replay/live
      // boundary the live gate below reads, and notifies the consumer.
      source.addEventListener('caught-up', (raw: MessageEvent<string>) => {
        const { seq } = JSON.parse(raw.data) as { seq: number };
        caughtUpSeq.current = seq;
        config.onCaughtUp?.(seq);
      });

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

        // Replayed frames patch state above but never toast. The caught-up marker
        // gives the exact replay/live boundary; without it (older servers) fall
        // back to the highWaterSeq snapshot heuristic, or toast everything.
        const live =
          caughtUpSeq.current !== null
            ? Number(raw.lastEventId) > caughtUpSeq.current
            : config.highWaterSeq === undefined
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

    // Seed peerPresent from the snapshot once the cache loads, so the peer dot
    // does not flicker through null before the first presence frame. The cache
    // may load after this provider mounts (the query lives in a child), so this
    // subscribes until the snapshot is present, then seeds once. A presence frame
    // that arrived first keeps its value (the updater only fills a null).
    const seededPeer = useRef(false);
    useEffect(() => {
      const project = config.initialPeerPresence;
      if (!project || seededPeer.current) return;
      const key = config.queryKey(subject, scope as Scope);
      const seed = () => {
        const cache = queryClient.getQueryData<Cache>(key);
        if (cache === undefined) return false;
        seededPeer.current = true;
        setPeerPresent((prev) => (prev === null ? project(cache) : prev));
        return true;
      };
      if (seed()) return;
      const unsub = queryClient.getQueryCache().subscribe(() => {
        if (seed()) unsub();
      });
      return unsub;
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
