<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Spinner

Use a spinner when the user is waiting on an operation including loading, downloading, uploading, processing, etc. that's indeterminate.
Spinners are displayed until content appears or a process is complete. This component should be used when the expected duration is more
than a second and the completion time is short (up to 5 seconds). A message can be used with a spinner to improve understanding.

## Examples

### Basic Spinner

Use when a process is indeterminate and no visible description is needed. An `aria-label` is required so screen readers announce the loading state.

```tsx
<Spinner aria-label="Loading" />
```

### Spinner With Description

Use `slotDescription` when a visible message helps the user understand what is being loaded. The description also satisfies the accessible name, so `aria-label` is not needed.

```tsx
<Spinner slotDescription="Loading models..." />
```

### Spinner Sizes

Pick the size to match the surrounding context: `small` (32px) for inline or in-button loading, `medium` (64px, default) for section-level waits, and `large` (128px) for full-page or empty-state loading.

```tsx
<div className="flex flex-col items-start gap-4">
    <Spinner size="small" aria-label="Loading" />
    <Spinner size="medium" aria-label="Loading" />
    <Spinner size="large" aria-label="Loading" />
</div>
```

## Props

| Prop                   | Type                              | Default    | Description                                                                                                                                      |
| ---------------------- | --------------------------------- | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| **slotDescription** \* | `ReactNode \| undefined \| never` | -          | An optional message that is displayed below the spinner. If not provided, an `aria-label` must be added instead.                                 |
| aria-label             | `never \| string`                 | -          | Defines a string value that labels the current element. Accessible label for the spinner. If `slotDescription` is provided do not use this prop. |
| description            | `ReactNode`                       | -          |                                                                                                                                                  |
| size                   | `"small" \| "medium" \| "large"`  | `'medium'` | The size of the Spinner - `small` - 32px - `medium` - 64px - `large` - 128px                                                                     |

`* = required prop`
