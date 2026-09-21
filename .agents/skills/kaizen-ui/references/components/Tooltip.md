<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Tooltip

A tooltip displays additional information when users hover or focus on an element.
To remove the delay on hover when switching between different tooltips, it is highly recommended
to wrap your application in a `TooltipProvider` component.

Built on the native Popover API and CSS Anchor Positioning for progressive
enhancement. Hover and focus interactions are managed with lightweight
JavaScript; uncontrolled tooltips also render a no-JS click target during SSR.

## Examples

### Basic Tooltip

```tsx
<Tooltip slotContent="Helpful tooltip text">
    <button type="button">Hover me</button>
</Tooltip>
```

### Positioning Tooltip

Combine `side` (`top` | `bottom` | `left` | `right`) and `align` (`start` | `center` | `end`) to control where the tooltip renders. Override the defaults when the trigger sits near a viewport edge or when wider tooltip content would clip with centered alignment.

```tsx
<Tooltip align="start" side="bottom" slotContent="Start-aligned tooltip">
    <button type="button">Aligned start</button>
</Tooltip>
```

### Disabled Trigger Tooltip

Wrap a disabled Button to explain why the action is unavailable. Tooltip routes hover and focus through a focusable wrapper since disabled controls don't fire pointer or focus events.

```tsx
<Tooltip slotContent="You don't have permission to perform this action.">
    <Button disabled>Save</Button>
</Tooltip>
```

### Conditional Tooltip

Toggle the disabled prop to suppress the tooltip without unmounting it. Useful when the tooltip should only appear in certain runtime states (e.g. `disabled={!isReadOnly}`).

```tsx
<Tooltip disabled slotContent="Hidden while disabled is true">
    <Button>Always enabled</Button>
</Tooltip>
```

### With Icon Trigger Tooltip

Pair a tooltip with an icon-only button to surface an accessible label without consuming layout space. Always provide aria-label on the trigger so assistive tech still announces it.

```tsx
<Tooltip slotContent="Get help with this feature">
    <Button aria-label="Help" kind="tertiary" size="small">
        <InfoCircle />
    </Button>
</Tooltip>
```

### Max Width Tooltip

Apply a max-width utility via className when the tooltip content is long enough to wrap. Prevents the tooltip from stretching across the viewport.

```tsx
<Tooltip
    className="max-w-[200px]"
    slotContent="This is a longer tooltip message that demonstrates how to constrain the maximum width so multi-line content stays readable."
>
    <button type="button">Long tooltip</button>
</Tooltip>
```

### With Provider Tooltip

Wrap your app in TooltipProvider to remove the hover delay when moving between adjacent tooltips.

```tsx
<TooltipProvider>
    <Tooltip slotContent="First tooltip">
        <button type="button">First</button>
    </Tooltip>
</TooltipProvider>
```

### Composed

Use composed primitives when you need full control over trigger and content rendering.

```tsx
<TooltipProvider>
    <TooltipRoot>
        <TooltipTrigger aria-label="Help">
            <InfoCircle />
        </TooltipTrigger>
        <TooltipContent>Composed tooltip content</TooltipContent>
    </TooltipRoot>
</TooltipProvider>
```

## Props

| Prop                 | Type                                     | Default    | Description                                                                                                                                                                                                             |
| -------------------- | ---------------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| align                | `"center" \| "start" \| "end"`           | `"center"` | The preferred alignment against the trigger. May change when collisions occur.                                                                                                                                          |
| asChild              | `boolean`                                | -          | When true, treats the immediate child as the host element, merging this component's props with the child's props. The child's children will be slotted into the component's defined content placement.                  |
| defaultOpen          | `boolean`                                | -          | The open state of the tooltip when it is initially rendered. Use when you do not need to control its open state.                                                                                                        |
| disabled             | `boolean`                                | -          | If true, the tooltip will be disabled and will not appear. Use this to conditionally render a tooltip.                                                                                                                  |
| id                   | `string`                                 | -          | A stable identifier used to derive the tooltip content id and CSS anchor name. Provide this when rendering in SSR-sensitive trees to ensure the generated ids remain consistent between server and client.              |
| onEscapeKeyDown      | `(event: KeyboardEvent) => void`         | -          | Event handler called when the escape key is down. It can be prevented by calling `event.preventDefault`.                                                                                                                |
| onOpenChange         | `(open: boolean) => void`                | -          | Event handler called when the open state of the tooltip changes.                                                                                                                                                        |
| onPointerDownOutside | `(event: Event) => void`                 | -          | Event handler called when a pointer event occurs outside the bounds of the component. It can be prevented by calling `event.preventDefault`.                                                                            |
| open                 | `boolean`                                | -          | The controlled open state of the tooltip. Must be used in conjunction with `onOpenChange`.                                                                                                                              |
| openDelayDuration    | `number`                                 | `100`      | The duration from when the mouse enters a tooltip trigger until the tooltip opens.                                                                                                                                      |
| positionAnchor       | `string`                                 | -          | The CSS anchor name to position this tooltip against. This should match the `anchorName` prop on the corresponding `TooltipTrigger`. When not provided, uses the auto-generated anchor name from `TooltipRoot` context. |
| side                 | `"right" \| "bottom" \| "left" \| "top"` | `"top"`    | The preferred side of the trigger to render the tooltip.                                                                                                                                                                |
| slotContent          | `ReactNode`                              | -          | The content of the tooltip. The Tooltip component takes care of the text styling and spacing for you. You may wish to limit the width of the tooltip to ensure readability.                                             |
