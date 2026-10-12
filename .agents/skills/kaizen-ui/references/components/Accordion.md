<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Accordion

A multi-section expand/collapse component for organizing content into collapsible panels. Supports single-open and multi-open modes.

## Examples

### Basic Accordion

By default the accordion allows for a single item to be open at a time. Set `multiple` to allow multiple items to be open at a time.

```tsx
<Accordion
    // defaults to single selection mode
    defaultValue="1"
    items={[
        {
            slotTrigger: 'Item 1',
            slotContent: 'Lorem ipsum dolor sit amet, consectetur adipiscing elit.',
            value: '1'
        },
        { slotTrigger: 'Item 2', slotContent: '...', value: '2' }
    ]}
/>
```

### Default Open Multiple Items Accordion

```tsx
<Accordion
    defaultValue={['1', '2']}
    items={[
        { slotTrigger: 'Item 1', slotContent: '...', value: '1' },
        { slotTrigger: 'Item 2', slotContent: '...', value: '2' }
    ]}
    multiple
/>
```

### With Icon In Trigger

Use gap="inherit" to use the Accordion defined gap for the icon and text.

```tsx
<Accordion
    items={[
        {
            slotTrigger: (
                <Flex align="center" gap="inherit">
                    <Icon /> Item 1
                </Flex>
            ),
            slotContent: '...',
            value: '1'
        },
        {
            slotTrigger: (
                <Flex align="center" gap="inherit">
                    <Icon /> Item 2
                </Flex>
            ),
            slotContent: '...',
            value: '2'
        }
    ]}
/>
```

### Composed

```tsx
<AccordionRoot>
    <AccordionItem value="1">
        <AccordionTrigger>Section 1</AccordionTrigger>
        <AccordionContent>Content for section 1</AccordionContent>
    </AccordionItem>
    <AccordionItem value="2">
        <AccordionTrigger>
            <Flex align="center" gap="inherit">
                <Icon />
                Section 2
            </Flex>
        </AccordionTrigger>
        <AccordionContent>Content for section 2</AccordionContent>
    </AccordionItem>
</AccordionRoot>
```

## Props

| Prop          | Type                                                                                                                        | Default | Description                                                                                                                                                                                                                                                                   |
| ------------- | --------------------------------------------------------------------------------------------------------------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **items** \*  | `{ slotTrigger: ReactNode; slotContent: ReactNode; value: string; disabled: boolean; chevronPosition: "start" \| "end" }[]` | -       | The items to render in the accordion.                                                                                                                                                                                                                                         |
| asChild       | `boolean`                                                                                                                   | -       | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                 |
| collapsible   | `boolean \| never`                                                                                                          | `true`  | For single item accordions, this prop controls whether the open item can be clicked to close it. When `collapsible` is false open items cannot be closed by clicking on them. This prop is only applicable to single item accordions, and is ignored when `multiple` is true. |
| defaultValue  | `string \| string[]`                                                                                                        | -       | The default open item(s) in the accordion.                                                                                                                                                                                                                                    |
| disabled      | `boolean`                                                                                                                   | -       | If true, the accordion will be disabled and interaction will be prevented.                                                                                                                                                                                                    |
| multiple      | `false \| true`                                                                                                             | `false` | When `multiple` is true the Accordion allows for multiple accordion items to be open at a time.                                                                                                                                                                               |
| name          | `string`                                                                                                                    | -       | A shared name for all accordion items. When provided with `multiple=false`, enables native browser single-select behavior for no-JS scenarios.                                                                                                                                |
| onValueChange | `(value: string) => void \| (value: string[]) => void`                                                                      | -       | Callback triggered when accordion items are opened or closed.                                                                                                                                                                                                                 |
| value         | `string \| string[]`                                                                                                        | -       | The currently open item(s) in the accordion. When setting this prop, you must also set the `onValueChange` prop to control the state of the accordion.                                                                                                                        |

`* = required prop`
