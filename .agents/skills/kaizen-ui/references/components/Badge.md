<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Badge

A small color-coded label for indicating status, category, or metadata at a glance. Badges are
non-interactive - they display information, not trigger actions.

## Notes

- Always use Badge (not plain text) to display entity status. Use a semantic color (e.g., color="green" for healthy, color="warning" for degraded).

- Use kind="solid" for status badges in detail views, side panels, and status bars. Use kind="outline" only for secondary/de-emphasized contexts like card metadata.

- Prefer label-only badges without icons for status (Healthy, Degraded, Pending). Icons on status badges add visual noise without information.

## Examples

### Basic Badge

```tsx
<Grid cols={2} gap="3" style={{ placeItems: 'center' }}>
    <Badge color="blue" kind="solid">
        Badge Text
    </Badge>
    <Badge color="blue">Badge Text</Badge>
    <Badge color="green" kind="solid">
        Badge Text
    </Badge>
    <Badge color="green">Badge Text</Badge>
    <Badge color="yellow" kind="solid">
        Badge Text
    </Badge>
    <Badge color="yellow">Badge Text</Badge>
    <Badge color="red" kind="solid">
        Badge Text
    </Badge>
    <Badge color="red">Badge Text</Badge>
</Grid>
```

### With Icon

```tsx
<Badge color="purple">
    <Shield /> Encrypted
</Badge>
```

### As Link

```tsx
<Badge asChild>
    <a href="/">Badge as a Link</a>
</Badge>
```

### With Truncation

Badge text should be short and concise. By default, overflowing content wraps so it stays readable. If you must truncate (e.g., a constrained layout with unpredictable content), wrap the text in a span with truncation styles.

```tsx
<Badge>
    <Shield />
    <span className="truncate">Long text that will be truncated</span>
</Badge>
```

## Props

| Prop    | Type                                                                     | Default     | Description                                                                                                                                                                           |
| ------- | ------------------------------------------------------------------------ | ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild | `boolean`                                                                | -           | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                         |
| color   | `"blue" \| "green" \| "red" \| "yellow" \| "purple" \| "teal" \| "gray"` | `"blue"`    | The badge color. Use a semantic color to convey meaning (e.g., color="green" for healthy, color="yellow" for degraded).                                                               |
| kind    | `"outline" \| "solid"`                                                   | `"outline"` | The style of the badge. Use kind="solid" for status badges in detail views, side panels, and status bars. Use kind="outline" for secondary/de-emphasized contexts like card metadata. |
