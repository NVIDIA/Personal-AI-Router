<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Label

A label component for forms and inputs. If you're using this component for a form field, just
use the FormField component instead. This component is mostly intended for internal use.

## Examples

### Basic Label

Use when labeling a form input to provide accessible context for the control.

```tsx
<Label htmlFor="email">Email Address</Label>
```

### Disabled Label

Use when the associated input is disabled to visually communicate the inactive state.

```tsx
<Label htmlFor="email" disabled>
    Email Address
</Label>
```

### Small Label

Use in compact layouts or alongside small-sized inputs where space is limited.

```tsx
<Label htmlFor="email" size="small">
    Email Address
</Label>
```

### Label With Icon

Use when the label needs an inline help affordance. Label accepts arbitrary children so an icon or tooltip can sit alongside the text.

```tsx
<Label htmlFor="email">
    Email Address
    <Tooltip slotContent="We use this to send account confirmations.">
        <InfoCircle />
    </Tooltip>
</Label>
```

## Props

| Prop     | Type                             | Default    | Description                                                                                                                   |
| -------- | -------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------------------------- |
| asChild  | `boolean`                        | -          | When true, renders the immediate child instead of the default element, merging this component's props with the child's props. |
| disabled | `boolean`                        | -          | Sets the label into a disabled state. Adjusting styles and removing click events.                                             |
| htmlFor  | `string`                         | -          | The id of the element the label is associated with.                                                                           |
| size     | `"small" \| "medium" \| "large"` | `"medium"` | The size of the label.                                                                                                        |
