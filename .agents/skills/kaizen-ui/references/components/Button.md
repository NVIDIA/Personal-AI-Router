<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Button

A clickable element that triggers an action. Use specific verb + noun labels instead of vague text.

## Notes

- For page-level CTAs, PageHeader slotActions, empty-state CTAs, and modal primary actions, use color="brand". A bare Button without color="brand" defaults to neutral, which is visually indistinguishable from secondary buttons.

- For destructive actions (Delete, Terminate, Revoke), use kind="primary" color="danger" and pair with a confirmation Modal.

- "Clear all" / "Clear filters" buttons should use kind="tertiary" and be placed alongside the filter chips they reset, not in the discovery toolbar.

- In confirmation modals, action button is trailing (right): [Cancel] [Action] left-to-right.

## Examples

### Basic Button

```tsx
<Flex direction="col" gap="2">
    <Button kind="primary" color="brand">
        Button
    </Button>
    <Button kind="secondary" color="neutral">
        Button
    </Button>
    <Button color="danger">Button</Button>
</Flex>
```

### With Icon Button

```tsx
<Button>
    <Document />
    Add Document
</Button>
```

### Icon Only Button

```tsx
<Button aria-label="Add document">
    <Document />
</Button>
```

### Primary Action Button

Preferred default for primary actions: primary & brand. Use the primary action button for the most important action in a context. It should be used sparingly and only for the most important actions(submit, create, deploy, save, etc)

```tsx
<Button color="brand">Create cluster</Button>
```

### Secondary Action Button

Preferred default for secondary actions: secondary & neutral. Use the secondary action button for actions that are not the most important in a context. It should be used for actions that are not the most important in a context(cancel, close, etc)

```tsx
<Button kind="secondary">Deploy to GPU</Button>
```

### Tertiary Or Inline Action Button

Preferred default for tertiary/inline actions: tertiary & neutral. For example - edit, configure, kebab menu triggers, etc

```tsx
<Button kind="tertiary">View logs</Button>
```

### Destructive Action Button

Preferred default for destructive actions: primary & danger. Use the destructive action button for actions that are destructive and cannot be undone.

```tsx
<Button color="danger">Delete cluster</Button>
```

### Destructive But Revertible Action Button

Preferred default for destructive but reversible actions: secondary & danger. For example - remove from list, unassign, etc.

```tsx
<Button color="danger" kind="secondary">
    Remove member
</Button>
```

### Button As Link

```tsx
<Button asChild>
    <a href="https://example.com">Link</a>
</Button>
```

### Button With Truncation

Button text should be short and concise, and if not text will wrap to the next line to ensure users can read the text. If you prefer to truncate the text you should set the `title` prop.

```tsx
<Button title="Download the full results as a CSV file">
    <span className="truncate">Download the full results as a CSV file</span>
</Button>
```

## Props

| Prop     | Type                                       | Default     | Description                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| -------- | ------------------------------------------ | ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild  | `boolean`                                  | -           | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                                                                                                                                                                           |
| color    | `"brand" \| "neutral" \| "danger"`         | `"neutral"` | The color variant of the button - "brand" - Used for primary actions, suitable for most use cases. Use for page-level CTAs, PageHeader slotActions, empty-state CTAs, and modal primary actions. - "neutral" - Used for regular actions and suitable for most use cases - "danger" - Used for destructive actions. Typically used for actions that cannot be undone, such as delete or remove. Pair with a confirmation Modal for irreversible actions. |
| disabled | `boolean`                                  | -           | Disables the button                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| kind     | `"primary" \| "secondary" \| "tertiary"`   | `"primary"` | The kind of button. - "primary" - The most important call-to-action on the page. Only one primary button should be displayed per context. It can be paired with secondary and tertiary buttons. - "secondary" - Used for regular actions and suitable for most use cases. - "tertiary" - Used for low-priority or supplemental actions. It can be paired with other buttons or displayed alone.                                                         |
| size     | `"small" \| "tiny" \| "medium" \| "large"` | `"medium"`  | The size of the button. - "large" - Best for the main call-to-action for the page - "medium" - Suitable for most use-cases - "small" - Ideal for compact layouts with limited space and less significant actions - "tiny" - Perfect for the compact, dense layouts, often used in tables to maximize horizontal space in cells                                                                                                                          |
