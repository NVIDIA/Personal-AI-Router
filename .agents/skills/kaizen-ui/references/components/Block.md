<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Block

A primitive div component. This component provides a convenient way to create elements with
consistent spacing and overflow handling.

## Examples

### Basic Block

```tsx
<Block
    style={{
        height: '100px',
        width: '200px',
        border: '3px dotted var(--border-color-base)'
    }}
/>
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

| Prop          | Type                                                    | Default | Description                                                                                                                                               |
| ------------- | ------------------------------------------------------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild       | `boolean`                                               | -       | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                             |
| gap           | `SpacingScaleUnion`                                     | -       | Sets spacing between flex and grid items.                                                                                                                 |
| overflow      | `"hidden" \| "auto" \| "clip" \| "scroll" \| "visible"` | -       | Sets content overfow handling. See https://developer.mozilla.org/en-US/docs/Web/CSS/overflow for more.                                                    |
| overflowX     | `"hidden" \| "auto" \| "clip" \| "scroll" \| "visible"` | -       | Sets content overfow handling for a block level element's left and right edges. See https://developer.mozilla.org/en-US/docs/Web/CSS/overflow-x for more. |
| overflowY     | `"hidden" \| "auto" \| "clip" \| "scroll" \| "visible"` | -       | Sets content overfow handling for a block level element's top and bottom edges. See https://developer.mozilla.org/en-US/docs/Web/CSS/overflow-y for more. |
| padding       | `SpacingScaleUnion`                                     | -       | Sets padding.                                                                                                                                             |
| paddingBottom | `SpacingScaleUnion`                                     | -       | Sets bottom padding.                                                                                                                                      |
| paddingLeft   | `SpacingScaleUnion`                                     | -       | Sets left padding.                                                                                                                                        |
| paddingRight  | `SpacingScaleUnion`                                     | -       | Sets right padding.                                                                                                                                       |
| paddingTop    | `SpacingScaleUnion`                                     | -       | Sets top padding.                                                                                                                                         |
| paddingX      | `SpacingScaleUnion`                                     | -       | Sets horizontal padding.                                                                                                                                  |
| paddingY      | `SpacingScaleUnion`                                     | -       | Sets vertical padding.                                                                                                                                    |
| textOverflow  | `"clip" \| "ellipsis"`                                  | -       | Sets text overflow handling. See https://developer.mozilla.org/en-US/docs/Web/CSS/text-overflow for more.                                                 |
| textWrap      | `"wrap" \| "nowrap" \| "balance" \| "pretty"`           | -       | Sets text wrap. See https://developer.mozilla.org/en-US/docs/Web/CSS/text-wrap for more.                                                                  |
| truncate      | `boolean`                                               | -       | Enables text truncation handling. Same as setting `{ wrap: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }`                                      |
