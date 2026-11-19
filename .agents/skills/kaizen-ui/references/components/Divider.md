<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Divider

A visual separator that creates a clear distinction between content sections

## Examples

### Basic Divider

```tsx
<Flex className="w-full" direction="col" gap="3">
    <Divider />
    <Divider width="medium" />
    <Divider width="large" />
</Flex>
```

### With Text

```tsx
<Flex align="center" gap="density-lg">
    <Divider />
    <Text kind="label/semibold/md">Text</Text>
    <Divider />
</Flex>
```

### Vertical Divider

```tsx
<Divider orientation="vertical" />
```

### Composed

```tsx
<DividerRoot paddingY="2">
    <DividerElement orientation="horizontal" width="medium" />
</DividerRoot>
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

| Prop          | Type                             | Default        | Description                                      |
| ------------- | -------------------------------- | -------------- | ------------------------------------------------ |
| asChild       | `boolean`                        | `false`        | Whether to render the divider as a child element |
| orientation   | `"horizontal" \| "vertical"`     | `"horizontal"` | The orientation of the divider element           |
| padding       | `SpacingScaleUnion`              | -              | Sets padding.                                    |
| paddingBottom | `SpacingScaleUnion`              | -              | Sets bottom padding.                             |
| paddingLeft   | `SpacingScaleUnion`              | -              | Sets left padding.                               |
| paddingRight  | `SpacingScaleUnion`              | -              | Sets right padding.                              |
| paddingTop    | `SpacingScaleUnion`              | -              | Sets top padding.                                |
| paddingX      | `SpacingScaleUnion`              | -              | Sets horizontal padding.                         |
| paddingY      | `SpacingScaleUnion`              | -              | Sets vertical padding.                           |
| width         | `"small" \| "medium" \| "large"` | `"small"`      | The width/thickness of the divider element       |
