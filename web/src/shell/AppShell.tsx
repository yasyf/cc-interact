// The layout substrate: the .app column with optional header / notifications /
// footer bands and a .body row of .sidebar + .main. Owns layout chrome only;
// the consumer fills every slot with its own domain components.

import type { ReactNode } from 'react';

export interface AppShellProps {
  header?: ReactNode;
  notifications?: ReactNode;
  sidebar?: ReactNode;
  main: ReactNode;
  footer?: ReactNode;
}

export function AppShell({ header, notifications, sidebar, main, footer }: AppShellProps) {
  return (
    <div className="app">
      {header}
      {notifications}
      <div className="body">
        {sidebar !== undefined && <div className="sidebar">{sidebar}</div>}
        <main className="main">{main}</main>
      </div>
      {footer}
    </div>
  );
}
