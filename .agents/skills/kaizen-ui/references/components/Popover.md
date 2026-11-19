<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Popover

Displays rich content in a portal, triggered by a button click. Use a popover
to display specific information, options, or actions related to an element on
the page.

Built on the native Popover API and CSS Anchor Positioning for progressive
enhancement. The trigger uses `popovertarget` so toggling works without
JavaScript. Positioning is handled entirely in CSS via `anchor-name` /
`position-anchor`.

## Examples

### Basic Popover

Use when you need to show supplementary information triggered by a button click without navigating away.

```tsx
<Popover slotContent={<p>Additional context about this item.</p>}>
    <Button>More Info</Button>
</Popover>
```

### Positioning Popover

Combine `side` (`top` | `bottom` | `left` | `right`) and `align` (`start` | `center` | `end`) to control where the popover renders relative to its trigger. Override the defaults when the trigger sits near a viewport edge or sibling content would otherwise overlap.

```tsx
<Popover side="right" align="end" slotContent={<p>Right side, end aligned</p>}>
    <Button>Open</Button>
</Popover>
```

### With Chevron Popover

Add `AnimatedChevron` inside the trigger to give a visual hint that the button toggles an open/closed surface.

```tsx
<Popover slotContent={<p>Additional context about this item.</p>}>
    <Button kind="secondary">
        Open Popover
        <AnimatedChevron />
    </Button>
</Popover>
```

### With Rich Content Popover

Compose layout primitives inside `slotContent` to render rich, structured content like profile cards. Keep the content focused — avoid overcrowding the popover.

```tsx
<Popover
    side="right"
    align="start"
    slotContent={
        <Stack gap="density-md" className="w-[260px] p-4">
            <Stack direction="row" gap="density-md" align="center">
                <Avatar fallback="JD" size="large" />
                <Stack gap="density-sm">
                    <Stack direction="row" gap="density-sm" align="center">
                        <Text kind="label/bold/md">John Doe</Text>
                        <Badge color="green" kind="solid">
                            Active
                        </Badge>
                    </Stack>
                    <Text kind="body/regular/sm">Product Designer</Text>
                </Stack>
            </Stack>
            <Stack direction="row" gap="density-sm">
                <Button kind="secondary" size="small">
                    View Profile
                </Button>
                <Button kind="primary" color="brand" size="small">
                    Message
                </Button>
            </Stack>
        </Stack>
    }
>
    <Button>View Team Member</Button>
</Popover>
```

### Modal Popover

Use when the popover content requires focused interaction and should prevent access to the rest of the page.

```tsx
<Popover
    modal
    slotContent={
        <div>
            <p>This popover traps focus and dims the background.</p>
            <Button>Action</Button>
        </div>
    }
>
    <Button>Open Modal Popover</Button>
</Popover>
```

### With Anchor Popover

Use `PopoverAnchor` when the popover should position against an element other than the trigger — for example, a row in a table where the action button lives in a different cell.

```tsx
<PopoverRoot>
    <Stack direction="row" gap="density-xl" align="center">
        <PopoverTrigger asChild>
            <Button>Trigger</Button>
        </PopoverTrigger>
        <PopoverAnchor>
            <Text kind="body/regular/md">Content anchors here</Text>
        </PopoverAnchor>
    </Stack>
    <PopoverContent>
        <p>This popover positions against the anchor, not the trigger.</p>
    </PopoverContent>
</PopoverRoot>
```

### Composed

Use composed primitives when you need full control over the popover trigger and content layout.

```tsx
<PopoverRoot>
    <PopoverTrigger asChild>
        <Button>Trigger</Button>
    </PopoverTrigger>
    <PopoverContent>
        <p>Composed popover content using primitives directly.</p>
    </PopoverContent>
</PopoverRoot>
```

## Props

| Prop                 | Type                                     | Default    | Description                                                                                                                                                                                                                                |
| -------------------- | ---------------------------------------- | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **slotContent** \*   | `ReactNode`                              | -          | The content rendered within the popover                                                                                                                                                                                                    |
| align                | `"center" \| "start" \| "end"`           | `"center"` | The alignment of the popover content relative to the trigger.                                                                                                                                                                              |
| defaultOpen          | `boolean`                                | -          | The open state of the popover when it is initially rendered. Use when you do not need to control its open state.                                                                                                                           |
| disabled             | `boolean`                                | -          | Disables the popover trigger                                                                                                                                                                                                               |
| id                   | `string`                                 | -          | A stable identifier used to derive the popover content id and CSS anchor name. Provide this when rendering in SSR-sensitive trees to ensure the generated ids remain consistent between server and client.                                 |
| modal                | `boolean`                                | `false`    | The modality of the popover. When set to true, interaction with outside elements will be disabled and only popover content will be visible to screen readers. Modality is disabled by default to minimize performance impact.              |
| onCloseAutoFocus     | `(event: Event) => void`                 | -          | Event handler called when focus moves to the trigger after closing. It can be prevented by calling `event.preventDefault`.                                                                                                                 |
| onEscapeKeyDown      | `(event: KeyboardEvent) => void`         | -          | Event handler called when the escape key is down. It can be prevented by calling `event.preventDefault`.                                                                                                                                   |
| onInteractOutside    | `(event: Event) => void`                 | -          | Event handler called when an interaction event occurs outside the bounds of the component. It can be prevented by calling `event.preventDefault`.                                                                                          |
| onOpenAutoFocus      | `(event: Event) => void`                 | -          | Event handler called when focus moves into the component after opening. It can be prevented by calling `event.preventDefault`.                                                                                                             |
| onOpenChange         | `(open: boolean) => void`                | -          | Event handler called when the open state of the popover changes.                                                                                                                                                                           |
| onPointerDownOutside | `(event: Event) => void`                 | -          | Event handler called when a pointer event occurs outside the bounds of the component. It can be prevented by calling `event.preventDefault`.                                                                                               |
| open                 | `boolean`                                | -          | The controlled open state of the popover. Must be used in conjunction with `onOpenChange`.                                                                                                                                                 |
| positionAnchor       | `string`                                 | -          | The CSS anchor name to position this popover against. This should match the `anchorName` prop on the corresponding `PopoverTrigger` or `PopoverAnchor`. When not provided, uses the auto-generated anchor name from `PopoverRoot` context. |
| side                 | `"right" \| "bottom" \| "left" \| "top"` | `"bottom"` | The preferred side of the trigger to render against when open. Will be reversed when collisions occur.                                                                                                                                     |

`* = required prop`
