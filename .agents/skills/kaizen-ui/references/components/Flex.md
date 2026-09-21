<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Flex

A primitive flex component.

## Notes

- Instead of using this component, just use `div` with Tailwind classes

## Examples

### Basic Flex

Use a basic row layout when items should flow horizontally with consistent spacing.

```tsx
<Flex gap="2" padding="1" style={{ border: '1px dotted var(--border-color-base)' }}>
    <Flex padding="1" style={{ flex: 1, border: '1px dotted var(--border-color-base)' }}>
        Item A
    </Flex>
    <Flex padding="1" style={{ flex: 2, border: '1px dotted var(--border-color-base)' }}>
        Item B
    </Flex>
    <Flex padding="1" style={{ flex: 1, border: '1px dotted var(--border-color-base)' }}>
        Item C
    </Flex>
</Flex>
```

### Column Flex

Use a column layout with centered alignment when stacking items vertically within a constrained width.

```tsx
<Flex direction="col" align="center" gap="2">
    <div>Item A</div>
    <div>Item B</div>
    <div>Item C</div>
</Flex>
```

### Wrapping Flex

Use wrapping when items should flow to the next line rather than overflowing their container.

```tsx
<Flex gap="2" wrap="wrap">
    <div>Item A</div>
    <div>Item B</div>
    <div>Item C</div>
    <div>Item D</div>
    <div>Item E</div>
</Flex>
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
| direction     | `"col" \| "row" \| "column" \| "row-reverse" \| "col-reverse" \| "column-reverse"`           | `"row"`     | Sets the direction in which child items are placed in the flex container. See https://developer.mozilla.org/en-US/docs/Web/CSS/flex-direction for more.         |
| gap           | `SpacingScaleUnion`                                                                          | -           | Sets spacing between flex and grid items.                                                                                                                       |
| justify       | `"center" \| "start" \| "end" \| "normal" \| "stretch" \| "between" \| "around" \| "evenly"` | `"start"`   | Sets distribution of space between and around content items along the main axis. See https://developer.mozilla.org/en-US/docs/Web/CSS/justify-content for more. |
| padding       | `SpacingScaleUnion`                                                                          | -           | Sets padding.                                                                                                                                                   |
| paddingBottom | `SpacingScaleUnion`                                                                          | -           | Sets bottom padding.                                                                                                                                            |
| paddingLeft   | `SpacingScaleUnion`                                                                          | -           | Sets left padding.                                                                                                                                              |
| paddingRight  | `SpacingScaleUnion`                                                                          | -           | Sets right padding.                                                                                                                                             |
| paddingTop    | `SpacingScaleUnion`                                                                          | -           | Sets top padding.                                                                                                                                               |
| paddingX      | `SpacingScaleUnion`                                                                          | -           | Sets horizontal padding.                                                                                                                                        |
| paddingY      | `SpacingScaleUnion`                                                                          | -           | Sets vertical padding.                                                                                                                                          |
| wrap          | `"wrap" \| "nowrap" \| "wrap-reverse"`                                                       | `"nowrap"`  | Sets the wrapping behavior of the flex container. See https://developer.mozilla.org/en-US/docs/Web/CSS/flex-wrap for more.                                      |
