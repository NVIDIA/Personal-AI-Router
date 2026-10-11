<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# StatusMessage

Use a status message when there is no data or content available. This helps inform users of the
current status and suggests next steps when appropriate.

## Notes

- The primary CTA in an empty-state StatusMessage goes in slotFooter and should use Button with kind="primary" color="brand".

## Examples

### Basic Status Message

Use when a page or section has no content to display, such as empty search results or a blank state.

```tsx
<StatusMessage
    slotMedia={<Icon />}
    slotHeading="No Results Found"
    slotSubheading="Try adjusting your search or filter criteria."
/>
```

### Small Status Message

Use for small areas of a page or individual components like cards or side panels.

```tsx
<StatusMessage
    size="small"
    slotMedia={<Icon />}
    slotHeading="No Items"
    slotSubheading="This list is empty."
/>
```

### With Footer Status Message

Use when the empty state has actionable next steps the user can take.

```tsx
<StatusMessage
    slotMedia={<Icon />}
    slotHeading="Forbidden"
    slotSubheading="You're not authorized to access this page."
    slotFooter={
        <>
            <Button kind="tertiary">Back Home</Button>
            <Button color="brand">Request Access</Button>
        </>
    }
/>
```

### With Image Media Status Message

Use an illustration instead of an icon for empty states that benefit from a richer visual; size the image explicitly since `slotMedia` only auto-sizes SVGs.

```tsx
<StatusMessage
    slotMedia={
        <img
            src="/kaizen-ui-foundations/images/placeholder.png"
            alt="Empty team illustration"
            style={{ height: '160px', width: '160px' }}
        />
    }
    slotHeading="No Team Members Yet"
    slotSubheading="Start building your team by inviting colleagues."
    slotFooter={<Button color="brand">Invite</Button>}
/>
```

### Composed

```tsx
<StatusMessageRoot size="medium">
    <StatusMessageMedia>
        <Icon />
    </StatusMessageMedia>
    <StatusMessageHeader>
        <StatusMessageHeading>No Team Members Yet</StatusMessageHeading>
        <StatusMessageSubheading>
            Start building your team by inviting colleagues.
        </StatusMessageSubheading>
    </StatusMessageHeader>
    <StatusMessageFooter>
        <Button color="brand">Invite</Button>
    </StatusMessageFooter>
</StatusMessageRoot>
```

## Props

| Prop               | Type                  | Default    | Description                                                                                                                                                                                                                                                                                    |
| ------------------ | --------------------- | ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **slotHeading** \* | `ReactNode`           | -          | The content to render in the heading. Rendered with `body/bold/2xl` or `body/bold/xl` typography styles depending on the size of the status message.                                                                                                                                           |
| asChild            | `boolean`             | -          | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                  |
| size               | `"small" \| "medium"` | `"medium"` | The size of the component. This will affect the size of the headig and subheading and the spacing between elements. - `small`: Use for small areas of a page or individual components (i.e., card, side panel) - `medium`: Use for large areas of the page (i.e., no search results. 404 page) |
| slotFooter         | `ReactNode`           | -          | The content to render in the footer. Handles the spacing and layout of footer items for you.                                                                                                                                                                                                   |
| slotMedia          | `ReactNode`           | -          | Optional media content to render in the status message. Use this to display an icon or image. This handles sizing and coloring of SVGs automatically.                                                                                                                                          |
| slotSubheading     | `ReactNode`           | -          | The content to render in the heading. Rendered with `body/regular/md` typography styles.                                                                                                                                                                                                       |

`* = required prop`
