<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# StatusIndicator

A status indicator is a simple circle that is ephemeral and used to signal new changes or, occasionally, status.

## Examples

### Status Indicator Colors

Map the `color` prop to semantic meaning: blue for new/info, red for error, yellow for warning, and green for success or healthy. Also supports custom colors via CSS.

```tsx
<Flex direction="col" gap="2">
    <StatusIndicator color="blue" />
    <StatusIndicator color="red" />
    <StatusIndicator color="yellow" />
    <StatusIndicator color="green" />
    <StatusIndicator className="bg-accent-teal" />
</Flex>
```

### Status Indicator Sizes

Scale the `size` prop to match surrounding content — smaller for dense rows and inline use, larger for prominent dashboards or hero areas.

```tsx
<Flex align="center" gap="2">
    <StatusIndicator size="small" />
    <StatusIndicator size="medium" />
    <StatusIndicator size="large" />
    <StatusIndicator size="xlarge" />
    <StatusIndicator size="xxlarge" />
</Flex>
```

### Status Indicator On Icon

Anchor the indicator over an icon with a relatively positioned wrapper to flag unread or actionable items like notifications.

```tsx
<div className="relative w-fit">
    <Bell />
    <StatusIndicator className="absolute top-0 right-0" size="medium" />
</div>
```

### Status Indicator On Avatar

Overlay an avatar with a green indicator to communicate a user's online or active presence.

```tsx
<div className="relative w-fit">
    <Avatar fallback="NV" />
    <StatusIndicator
        className="absolute right-0.5 bottom-0.5 translate-x-1/2"
        color="green"
        size="medium"
    />
</div>
```

### Status Indicator With Label

Pair the indicator with an inline text label whenever the status must be unambiguous and accessible to assistive tech — color alone is not enough.

```tsx
<Flex align="center" gap="2">
    <StatusIndicator color="green" size="medium" />
    <span>Stable</span>
</Flex>
```

## Props

| Prop    | Type                                                      | Default    | Description                                                                                                                   |
| ------- | --------------------------------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------------------------- |
| asChild | `boolean`                                                 | -          | When true, renders the immediate child instead of the default element, merging this component's props with the child's props. |
| color   | `"blue" \| "green" \| "red" \| "yellow"`                  | `"red"`    | The color of the status indicator                                                                                             |
| size    | `"small" \| "medium" \| "large" \| "xlarge" \| "xxlarge"` | `"medium"` | The size of the status indicator                                                                                              |
