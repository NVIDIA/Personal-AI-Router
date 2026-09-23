<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Stack

A container component for laying out content in a vertical or horizontal stack. This is a wrapper
around the {@link Flex} component.

## Notes

- DO NOT add a single div to the Stack component, it is expected to have multiple children in a Stack.

## Examples

### Basic Stack

```tsx
<Stack padding="1" gap="2" style={{ border: '3px dotted var(--border-color-base)' }}>
    <div>Item A</div>
    <div>Item B</div>
    <div>Item C</div>
</Stack>
```

### Horizontal Stack

```tsx
<Stack direction="row" gap="2">
    <div>Item A</div>
    <div>Item B</div>
    <div>Item C</div>
</Stack>
```

### With Divider Stack

Use slotDivider to automatically inject a separator between each child without manual repetition.

```tsx
<Stack gap="2" slotDivider={<Divider />}>
    <div>Section A</div>
    <div>Section B</div>
    <div>Section C</div>
</Stack>
```

### Aligned Stack

Use align to position children along the cross-axis (e.g. center) when items have varying widths in a column or heights in a row.

```tsx
<Stack gap="2" align="center">
    <div>Short</div>
    <div>A longer item</div>
    <div>Mid</div>
</Stack>
```

### Justified Stack

Use direction=row with justify=between to push siblings to opposite ends of a bar, the canonical pattern for toolbars and page headers.

```tsx
<Stack direction="row" justify="between">
    <div>Leading</div>
    <div>Trailing</div>
</Stack>
```

## Props

type SpacingScaleUnion

Prefer semantic density tokens that adapt to the active density theme (compact, standard, spacious):
"density-xxs" | "density-xs" | "density-sm" | "density-md" | "density-lg" | "density-xl" | "density-2xl" | "density-3xl" | "density-4xl" | "density-5xl"

| Token       | Compact | Standard | Spacious |
| ----------- | ------- | -------- | -------- |
| density-xxs | 1px     | 2px      | 4px      |
| density-xs  | 2px     | 4px      | 6px      |
| density-sm  | 4px     | 6px      | 8px      |
| density-md  | 6px     | 8px      | 12px     |
| density-lg  | 8px     | 12px     | 16px     |
| density-xl  | 12px    | 16px     | 24px     |
| density-2xl | 16px    | 24px     | 32px     |
| density-3xl | 24px    | 32px     | 48px     |
| density-4xl | 32px    | 48px     | 64px     |
| density-5xl | 48px    | 64px     | 80px     |

Raw scale values (4px base unit — use only when density tokens don't fit):
"0" | "0.25" | "0.5" | "0.75" | "1" | "1.5" | "2" | "2.5" | "3" | "3.5" | "4" | "5" | "6" | "7" | "8" | "9" | "10" | "11" | "12" | "14" | "16" | "18" | "20" | "24" | "28" | "32" | "36" | "40" | "44" | "48" | "52" | "56" | "60" | "64" | "72" | "80" | "96" | "250" | "inherit" | "px"

| Prop          | Type                                                                                         | Default     | Description                                                                                                                                                     |
| ------------- | -------------------------------------------------------------------------------------------- | ----------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| align         | `"center" \| "start" \| "end" \| "baseline" \| "stretch"`                                    | `"stretch"` | Sets the alignment of child items along the block axis. See https://developer.mozilla.org/en-US/docs/Web/CSS/align-items for more.                              |
| asChild       | `boolean`                                                                                    | -           | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                   |
| direction     | `"col" \| "row" \| "column" \| "row-reverse" \| "col-reverse" \| "column-reverse"`           | `"col"`     | Sets the direction in which child items are placed in the flex container. See https://developer.mozilla.org/en-US/docs/Web/CSS/flex-direction for more.         |
| gap           | `SpacingScaleUnion`                                                                          | -           | Sets spacing between flex and grid items.                                                                                                                       |
| justify       | `"center" \| "start" \| "end" \| "normal" \| "stretch" \| "between" \| "around" \| "evenly"` | `"start"`   | Sets distribution of space between and around content items along the main axis. See https://developer.mozilla.org/en-US/docs/Web/CSS/justify-content for more. |
| padding       | `SpacingScaleUnion`                                                                          | -           | Sets padding.                                                                                                                                                   |
| paddingBottom | `SpacingScaleUnion`                                                                          | -           | Sets bottom padding.                                                                                                                                            |
| paddingLeft   | `SpacingScaleUnion`                                                                          | -           | Sets left padding.                                                                                                                                              |
| paddingRight  | `SpacingScaleUnion`                                                                          | -           | Sets right padding.                                                                                                                                             |
| paddingTop    | `SpacingScaleUnion`                                                                          | -           | Sets top padding.                                                                                                                                               |
| paddingX      | `SpacingScaleUnion`                                                                          | -           | Sets horizontal padding.                                                                                                                                        |
| paddingY      | `SpacingScaleUnion`                                                                          | -           | Sets vertical padding.                                                                                                                                          |
| slotDivider   | `ReactNode`                                                                                  | -           | If provided, renders `slotDivider` between each child.                                                                                                          |
| wrap          | `"wrap" \| "nowrap" \| "wrap-reverse"`                                                       | `"nowrap"`  | Sets the wrapping behavior of the flex container. See https://developer.mozilla.org/en-US/docs/Web/CSS/flex-wrap for more.                                      |
