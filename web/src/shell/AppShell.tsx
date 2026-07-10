// The layout substrate: the .app column with optional header / footer bands and a
// .body row of .sidebar + .main. Owns layout chrome only; the consumer fills every
// slot with its own domain components. Floating overlays (the toast stack) render
// as siblings of AppShell, not through a band here.

import type { ReactNode } from 'react';

export interface AppShellProps {
  header?: ReactNode;
  sidebar?: ReactNode;
  main: ReactNode;
  footer?: ReactNode;
}

export function AppShell({ header, sidebar, main, footer }: AppShellProps) {
  return (
    <div className="app">
      {header}
      <div className="body">
        {sidebar !== undefined && <div className="sidebar">{sidebar}</div>}
        <main className="main">{main}</main>
      </div>
      {footer}
    </div>
  );
}
