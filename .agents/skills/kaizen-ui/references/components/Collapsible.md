<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Collapsible

A utility component that allows you to collapse and expand content.

## Examples

### Basic Collapsible

Use when content should be hidden by default to reduce visual noise but available on demand.

```tsx
<Collapsible slotTrigger="Trigger content">Hidden content revealed on expand.</Collapsible>
```

### Default Open Collapsible

```tsx
<Collapsible defaultOpen slotTrigger="Hide details">
    Content visible by default.
</Collapsible>
```

### With Icon In Trigger

```tsx
<Collapsible
    slotTrigger={
        <span className="inline-flex items-center gap-2">
            <ChevronDown />
            Header
        </span>
    }
>
    Content
</Collapsible>
```

### With Chevron Trigger

The AnimatedChevron component automatically reads the open/closed state from Collapsible.

```tsx
<Collapsible
    slotTrigger={
        <>
            <AnimatedChevron /> Toggle content
        </>
    }
>
    Content
</Collapsible>
```

### With Button Trigger

When using a Button in slotTrigger with asChild, render it as a non-interactive element like <div> or <span> to avoid nested interactive elements, allowing the native <summary> element to handle the toggle behavior.

```tsx
<Collapsible
    slotTrigger={
        <Button asChild kind="tertiary">
            <div>Toggle content</div>
        </Button>
    }
>
    Content
</Collapsible>
```

### Composed

Use composed primitives when you want to render the trigger and content apart from each other.

```tsx
<CollapsibleRoot defaultOpen>
    <CollapsibleTrigger>
        <Button asChild kind="tertiary">
            <div>
                Toggle content
                <AnimatedChevron />
            </div>
        </Button>
    </CollapsibleTrigger>
    <CollapsibleContent>Additional content - visible by default.</CollapsibleContent>
</CollapsibleRoot>
```

## Props

| Prop               | Type                      | Default | Description             |
| ------------------ | ------------------------- | ------- | ----------------------- |
| **children** \*    | `ReactNode`               | -       | The collapsible content |
| **slotTrigger** \* | `ReactNode`               | -       | The collapsible trigger |
| defaultOpen        | `boolean`                 | -       |                         |
| disabled           | `boolean`                 | -       |                         |
| onOpenChange       | `(open: boolean) => void` | -       |                         |
| open               | `boolean`                 | -       |                         |

`* = required prop`
