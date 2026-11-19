<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# AnimatedChevron

An internal utility component. Renders a chevron icon that indicates an open or closed state,
which is used in accordion- and menu-related components. Reads `data-state` attribute to determine the state.

## Examples

### Basic Usage

Reads open/closed state from `data-state="open"` or `data-state="closed"` from parent.

```tsx
<div data-state="open">
    <AnimatedChevron />
</div>
```

### Controlled Usage

Manually control the state with the `state` prop.

```tsx
<AnimatedChevron state="closed" />
```

### Usage With Collapsible

When used in most of our components, it automatically reads the open/closed state from the parent data attribute, such as Collapsible.

```tsx
<Collapsible
    slotTrigger={
        <button type="button">
            <AnimatedChevron />
        </button>
    }
>
    ...
</Collapsible>
```

## Props

| Prop  | Type                 | Default | Description                                                                                                                                       |
| ----- | -------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| state | `"open" \| "closed"` | -       | Used for determining whether the Chevron should point up or down, for elements that don't use data-state in the parent component (ex: InputShell) |
