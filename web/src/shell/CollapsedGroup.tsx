// A collapsible group: a header button toggling a body that mounts only while
// expanded, plus a cooperative read-only signal descendants read via
// useGroupReadOnly. Presentational only — the consumer fills header and body.

import { createContext, useContext, useState } from 'react';
import type { ReactNode } from 'react';

const ReadOnlyCtx = createContext(false);

export interface CollapsedGroupProps {
  header: ReactNode;
  children: ReactNode;
  defaultExpanded?: boolean;
  readOnly?: boolean;
  className?: string;
}

export function CollapsedGroup({
  header,
  children,
  defaultExpanded,
  readOnly,
  className,
}: CollapsedGroupProps) {
  const [expanded, setExpanded] = useState(defaultExpanded ?? false);
  const cls = ['cc-group'];
  if (expanded) cls.push('cc-group-expanded');
  if (readOnly) cls.push('cc-group-readonly');
  if (className) cls.push(className);

  return (
    <section className={cls.join(' ')}>
      <button
        type="button"
        className="cc-group-header"
        aria-expanded={expanded}
        onClick={() => setExpanded((v) => !v)}
      >
        <span className="cc-group-caret" aria-hidden="true">
          ▶
        </span>
        {header}
      </button>
      {expanded && (
        <div className="cc-group-body">
          <ReadOnlyCtx.Provider value={!!readOnly}>{children}</ReadOnlyCtx.Provider>
        </div>
      )}
    </section>
  );
}

export function useGroupReadOnly(): boolean {
  return useContext(ReadOnlyCtx);
}
