<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# List

A flexible list component that supports unordered, ordered, and icon lists.

## Examples

### Basic List

```tsx
<List items={['Design tokens', 'Components', 'Patterns']} />
```

### Ordered List

Use when items follow a specific sequence such as instructions or steps.

```tsx
<List kind="ordered" items={['Install the package', 'Import the component', 'Render it']} />
```

### With Custom Marker List

Override per-item markers with a JSX node (icon) or a string. Items left as plain strings keep the default marker for the list kind.

```tsx
<List
    items={[
        'Design tokens',
        {
            children: 'Components',
            slotMarker: <ChevronRight />
        },
        { children: 'Patterns', slotMarker: '•' }
    ]}
/>
```

### Composed

```tsx
<ListRoot kind="unordered">
    <ListItem>
        <ListItemMarker />
        Design tokens
    </ListItem>
    <ListItem>
        <ListItemMarker />
        Components
    </ListItem>
    <ListItem>
        <ListItemMarker />
        Patterns
    </ListItem>
</ListRoot>
```

## Props

| Prop         | Type                                                         | Default       | Description                                                      |
| ------------ | ------------------------------------------------------------ | ------------- | ---------------------------------------------------------------- |
| **items** \* | `string \| { children: ReactNode; slotMarker: ReactNode }[]` | -             | Array of list item objects with flexible content and attributes. |
| kind         | `"ordered" \| "unordered"`                                   | `"unordered"` | The type of list to display.                                     |

`* = required prop`
