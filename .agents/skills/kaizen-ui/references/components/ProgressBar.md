<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# ProgressBar

Use a progress bar to visually communicate processes that involve long wait times, such as
downloading, uploading, submitting, or loading data.

## Examples

### Basic Progress Bar

Use a determinate progress bar when the completion percentage is known.

```tsx
<ProgressBar aria-label="65% complete" value={65} />
```

### Indeterminate Progress Bar

Use an indeterminate progress bar when the duration of a process is unknown.

```tsx
<ProgressBar kind="indeterminate" aria-label="Loading..." />
```

### Sizes Progress Bar

Match the bar to its surroundings: `small` for inline or table row indicators, `medium` (default) for section-level progress, and `large` for prominent file uploads or page-level loading.

```tsx
<div className="flex flex-col gap-4">
    <ProgressBar aria-label="30% complete" value={30} size="small" />
    <ProgressBar aria-label="55% complete" value={55} size="medium" />
    <ProgressBar aria-label="80% complete" value={80} size="large" />
</div>
```

### Labeled Progress Bar

Pair the progress bar with a visible text label using `aria-labelledby` when users benefit from seeing the operation name and percentage alongside the bar.

```tsx
<div className="w-full">
    <div className="mb-1 flex justify-between">
        <span id="progress-bar-label">Uploading documents</span>
        <span aria-hidden="true">50%</span>
    </div>
    <ProgressBar aria-labelledby="progress-bar-label" value={50} />
</div>
```

## Props

| Prop              | Type                               | Default         | Description                                                                                                                                                                                           |
| ----------------- | ---------------------------------- | --------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **aria-label** \* | `string \| never`                  | -               | Defines a string value that labels the current element.                                                                                                                                               |
| **value** \*      | `number \| never`                  | `0`             | A `number` that indicates the current progress based on a percentage to 100. Numbers \>= 100 will show a full bar. i.e., if you wish the progress bar to be halfway filled, the number would be `50`. |
| aria-labelledby   | `never \| string`                  | -               | Identifies the element (or elements) that labels the current element.                                                                                                                                 |
| kind              | `"determinate" \| "indeterminate"` | `"determinate"` | The kind of progress bar. Can be either `determinate` which is static and shows the current progress, or `indeterminate` which is animated and does not show progress.                                |
| size              | `"small" \| "medium" \| "large"`   | `"medium"`      | The size of the progress bar.                                                                                                                                                                         |

`* = required prop`
