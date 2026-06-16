// A generic subject/scope context: the id of the thing on screen plus an opaque
// scope (the displayed slice). Built as a factory so the value type is exact,
// mirroring createEventStream.

import { createContext, useContext } from 'react';
import type { FC, ReactNode } from 'react';

export interface SubjectContextValue<Subject, Scope> {
  subject: Subject;
  scope: Scope;
}

export interface SubjectContext<Subject, Scope> {
  SubjectProvider: FC<{ value: SubjectContextValue<Subject, Scope>; children: ReactNode }>;
  useSubject: () => SubjectContextValue<Subject, Scope>;
}

export function createSubjectContext<Subject = string, Scope = void>(): SubjectContext<
  Subject,
  Scope
> {
  const Context = createContext<SubjectContextValue<Subject, Scope> | null>(null);

  const SubjectProvider: FC<{
    value: SubjectContextValue<Subject, Scope>;
    children: ReactNode;
  }> = ({ value, children }) => <Context.Provider value={value}>{children}</Context.Provider>;

  function useSubject(): SubjectContextValue<Subject, Scope> {
    const value = useContext(Context);
    if (!value) throw new Error('useSubject must be used within SubjectProvider');
    return value;
  }

  return { SubjectProvider, useSubject };
}
