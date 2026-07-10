// @cc-interact/react — opt-in browser client for cc-interact: the realtime
// transport, the query primitives, and the layout shell, all domain-agnostic.
// Consumers supply the domain (reduction, hooks, panels) on top.

import './base.css';

export { createEventStream } from './stream';
export type {
  EventStream,
  EventStreamConfig,
  EventStreamProviderProps,
  EventStreamValue,
  EventContext,
  StreamNotification,
  StreamToast,
  ToastKind,
} from './stream';

export { createQueryClient, request, scopedKey, useOptimisticMutation } from './query';
export type { OptimisticMutationConfig } from './query';
export { useFlip } from './query/flip';
export type { FlipOptions } from './query/flip';

export {
  AppShell,
  ConnectionFrame,
  ToastStack,
  createSubjectContext,
  CollapsedGroup,
  useGroupReadOnly,
} from './shell';
export type {
  AppShellProps,
  ConnectionFrameProps,
  ToastStackProps,
  SubjectContext,
  SubjectContextValue,
  CollapsedGroupProps,
} from './shell';
