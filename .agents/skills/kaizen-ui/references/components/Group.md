<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Group

Group is a utility component that joins items together visually.

## Examples

### Basic Group

Use a flush group to visually join sibling items without any spacing or dividers between them.

```tsx
<Group>
    <Badge>Item A</Badge>
    <Badge>Item B</Badge>
    <Badge>Item C</Badge>
</Group>
```

### Gap Group

Use a gap group when joined items need visible spacing between them while remaining a logical unit.

```tsx
<Group kind="gap">
    <Badge>Item A</Badge>
    <Badge>Item B</Badge>
    <Badge>Item C</Badge>
</Group>
```

### Border Group

Use a border group when items lack their own borders and need a visible separator to distinguish each entry.

```tsx
<Group kind="border">
    <Badge>Item A</Badge>
    <Badge>Item B</Badge>
    <Badge>Item C</Badge>
</Group>
```

### With Badges Group

Use flush with components that already have borders (Badges, Tags, Buttons) to conjoin them into a single visual unit with a 1px overlap.

```tsx
<Group kind="flush">
    <Badge kind="outline">Item 1</Badge>
    <Badge kind="outline">Item 2</Badge>
    <Badge kind="outline">Item 3</Badge>
</Group>
```

### With Tags Group

Use Group to lay out a removable filter chip as a label tag joined to a dismiss tag so each has its own click target while reading as one unit.

```tsx
<Group kind="flush">
    <Tag onClick={handleTagClick}>Filter Name</Tag>
    <Tag aria-label="Remove filter" onClick={handleTagRemove}>
        <Close />
    </Tag>
</Group>
```

### With Input And Button Group

Use to attach an action trigger (dropdown, generate, refresh) directly to a TextInput so the pair reads as one input control.

```tsx
<Group kind="flush">
    <TextInput placeholder="Enter value" />
    <Button aria-label="Open options" kind="secondary">
        <ChevronDown />
    </Button>
</Group>
```

### Single Child Group

Border radius is preserved with a single child, so groups can wrap collections of unknown length (e.g. server-fetched lists) without special-casing the empty / one-item case.

```tsx
<Group kind="flush">
    <Badge kind="outline">Single Item</Badge>
</Group>
```

## Props

| Prop    | Type                           | Default   | Description                                                                                                                                                                                                                                                                                         |
| ------- | ------------------------------ | --------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild | `boolean`                      | -         | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                       |
| kind    | `"gap" \| "flush" \| "border"` | `"flush"` | The kind of divider to render for the group. By default groups are rendered as "flush" groups with no divider. Use `gap` to render a group with a gap between items. Use `border` to render a group with a border between items. Use this when the group items don't have a border or clear ending. |
