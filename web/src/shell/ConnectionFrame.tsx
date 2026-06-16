// The connection pill: the slice of the stream that shows live / reconnecting
// chrome. Reads nothing from the stream context directly — the consumer feeds
// it `connected` from useEventStream().

export interface ConnectionFrameProps {
  connected: boolean;
  labels?: { on?: string; off?: string };
}

export function ConnectionFrame({ connected, labels }: ConnectionFrameProps) {
  const on = labels?.on ?? 'live';
  const off = labels?.off ?? 'reconnecting…';
  return (
    <span className={`conn conn-${connected ? 'on' : 'off'}`}>{connected ? on : off}</span>
  );
}
