// FLIP for any reorderable list: measure rows bearing data-flip-key on every
// render; when the rendered-order signature changes, translate rows from their
// old positions back to zero. Signature-based so an identical re-render is a
// no-op. The class names are parameterized so the consumer styles the
// transition under its own selectors.

import { useLayoutEffect, useRef } from 'react';
import type { RefObject } from 'react';

export interface FlipOptions {
  // Applied while a moved row eases back to position; the transition lives here.
  flipClass?: string;
  // Applied for the one-shot highlight on a moved row.
  movedClass?: string;
}

const DEFAULT_FLIP_CLASS = 'cc-flip';
const DEFAULT_MOVED_CLASS = 'cc-flip-moved';

export function useFlip(container: RefObject<HTMLElement | null>, options?: FlipOptions): void {
  const flipClass = options?.flipClass ?? DEFAULT_FLIP_CLASS;
  const movedClass = options?.movedClass ?? DEFAULT_MOVED_CLASS;
  const previous = useRef<{ signature: string; tops: Map<string, number> }>({
    signature: '',
    tops: new Map(),
  });

  useLayoutEffect(() => {
    const root = container.current;
    if (!root) return;
    // Tops are content-relative: the container scrolls, and a scroll does not
    // re-render, so viewport-relative tops would go stale and slide the whole
    // list by the scroll delta on the next move.
    const rootTop = root.getBoundingClientRect().top - root.scrollTop;
    const rows = [...root.querySelectorAll<HTMLElement>('[data-flip-key]')].map((el) => ({
      el,
      key: el.dataset.flipKey as string,
      top: el.getBoundingClientRect().top - rootTop,
    }));
    const signature = rows.map((r) => r.key).join('\n');
    const last = previous.current;
    previous.current = { signature, tops: new Map(rows.map((r) => [r.key, r.top])) };
    if (last.signature === signature || last.tops.size === 0) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

    const moved = rows.filter(({ el, key, top }) => {
      const before = last.tops.get(key);
      if (before === undefined || before === top) return false;
      el.classList.remove(flipClass, movedClass);
      el.style.transform = `translateY(${before - top}px)`;
      return true;
    });
    if (moved.length === 0) return;
    void root.offsetWidth; // reflow so the transition starts from the offset
    for (const { el } of moved) {
      el.classList.add(flipClass, movedClass);
      el.style.transform = '';
      el.addEventListener('transitionend', () => el.classList.remove(flipClass), { once: true });
      el.addEventListener('animationend', () => el.classList.remove(movedClass), { once: true });
    }
  });
}
