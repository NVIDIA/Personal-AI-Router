<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Tag

Tags are used to categorize, label, or group items using text and optional icons, providing users
with quick, contextual information.

## Notes

- Tags should be label-only by default. Do not add icons unless the tag represents a typed entity (e.g., a person tag with an avatar).

- Do not use Tag for multi-value table cells in dense data tables — pills compound into visual noise. Use plain text or Anchor components separated by commas instead.

## Examples

### Basic Tag

Use outline tags when you need a lighter visual weight, such as in dense layouts or alongside solid tags for hierarchy.

```tsx
<Grid cols={2} gap="3" style={{ placeItems: 'center' }}>
    <Tag color="blue">Tag Text</Tag>
    <Tag color="blue" kind="outline">
        Tag Text
    </Tag>
    <Tag color="green">Tag Text</Tag>
    <Tag color="green" kind="outline">
        Tag Text
    </Tag>
    <Tag color="yellow">Tag Text</Tag>
    <Tag color="yellow" kind="outline">
        Tag Text
    </Tag>
    <Tag color="red" disabled>
        Tag Text
    </Tag>
    <Tag color="red" disabled kind="outline">
        Tag Text
    </Tag>
</Grid>
```

### With Icon Tag

Use when an icon reinforces the tag's meaning, such as a status or category indicator.

```tsx
<Tag>
    <Icon />
    Tag Text
</Tag>
```

### Selected Tag

Use to indicate the tag is actively selected in a filter or multi-select context. Automatically triggered by children that are `:checked` or `[data-state="checked"]` - use a hidden checkbox or radio input to control the state.

```tsx
<Flex direction="col" gap="density-md">
    <Tag selected>Selected</Tag>
    <Tag asChild>
        <label>
            <input hidden type="checkbox" /> Checkbox
        </label>
    </Tag>
    <Tag asChild>
        <label>
            <input hidden type="radio" /> Radio
        </label>
    </Tag>
</Flex>
```

### Disabled Tag

Use to communicate that an interactive tag is temporarily unavailable; pair with explanatory copy when the reason is not obvious.

```tsx
<Tag disabled>Disabled</Tag>
```

### Read Only Tag

Use when the tag is informational only and should not be interactive, such as metadata labels. Tag will render as a `span` instead of a `button`.

```tsx
<Tag readOnly>Read Only</Tag>
```

### Colors Tag

Pick a `color` to communicate semantic meaning — typically `green` for success, `red` for errors, `yellow` for warnings, and `blue` (default) for neutral or informational chips.

```tsx
<Flex gap="density-md" wrap="wrap">
    <Tag color="blue">Blue</Tag>
    <Tag color="gray">Gray</Tag>
    <Tag color="green">Green</Tag>
    <Tag color="purple">Purple</Tag>
    <Tag color="red">Red</Tag>
    <Tag color="teal">Teal</Tag>
    <Tag color="yellow">Yellow</Tag>
</Flex>
```

### Density Tag

Use `compact` for dense data tables and filter bars, `spacious` for prominent filter chips or hero surfaces, and the default `standard` for general use. Note that density is also inherited from the parent container.

```tsx
<Flex align="center" gap="density-md">
    <Tag density="compact">Compact</Tag>
    <Tag>Standard</Tag>
    <Tag density="spacious">Spacious</Tag>
</Flex>
```

### Dismissible Tag

Use a single tag with a trailing close icon when clicking anywhere on the tag should remove it, such as a list of applied filter chips.

```tsx
<Tag aria-label="Remove filter">
    Filter
    <CloseIcon />
</Tag>
```

### Split Tag

Wrap two adjacent tags in `Group` when the label and a secondary action (e.g. dismiss, edit) need separate click targets.

```tsx
<Group>
    <Tag>Filter</Tag>
    <Tag aria-label="Remove filter">
        <CloseIcon />
    </Tag>
</Group>
```

### As Link Tag

Use `asChild` with an anchor when the tag should navigate; the anchor inherits tag styling while staying keyboard-accessible.

```tsx
<Tag asChild>
    <a href="/">Tag as Link</a>
</Tag>
```

### Truncated Tag

Tags wrap their content by default. Only wrap text in a `truncate` span when the tag width is constrained and wrapping is unacceptable; pair with a Tooltip so the full label stays reachable.

```tsx
<Tag title="Tag content that may be too long">
    <Icon />
    <span className="truncate">Tag content that may be too long</span>
</Tag>
```

## Props

| Prop     | Type                                                                     | Default   | Description                                                                                                                                                         |
| -------- | ------------------------------------------------------------------------ | --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild  | `boolean`                                                                | -         | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                       |
| color    | `"blue" \| "green" \| "red" \| "yellow" \| "purple" \| "teal" \| "gray"` | `"blue"`  |                                                                                                                                                                     |
| density  | `"compact" \| "standard" \| "spacious"`                                  | -         |                                                                                                                                                                     |
| kind     | `"outline" \| "solid"`                                                   | `"solid"` | The kind of tag. Can be either `solid` or `outline`.                                                                                                                |
| readOnly | `boolean`                                                                | -         | Whether the tag is read-only. This will prevent the tag from being interacted with. When `readOnly` is true, the tag renders as a `<span>` instead of a `<button>`. |
| selected | `boolean`                                                                | -         | Whether or not the tag is selected.                                                                                                                                 |
