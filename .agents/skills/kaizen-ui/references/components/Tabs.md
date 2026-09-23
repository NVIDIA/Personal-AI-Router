<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Tabs

A high-level tabs component that provides a simple way to create tabbed interfaces.
Wraps the lower-level composed tab components for ease of use.

## Examples

### Basic Tabs

Use when content is divided into related sections that users switch between.

```tsx
<Tabs
    items={[
        {
            children: 'Account',
            slotContent: 'Account settings content',
            value: 'account'
        },
        {
            children: 'Password',
            slotContent: 'Password settings content',
            value: 'password'
        },
        {
            children: 'Notifications',
            slotContent: 'Notification preferences',
            value: 'notifications'
        },
        {
            children: 'Disabled',
            slotContent: 'Disabled tab content',
            value: 'disabled',
            disabled: true
        },
        {
            children: 'Another',
            slotContent: 'Another tab content',
            value: 'another'
        }
    ]}
/>
```

### Secondary Tabs

Use for secondary-level tab navigation within a page section. `kind="tertiary"` renders a minimal text-only style for optional or supplementary content.

```tsx
<Tabs
    kind="secondary"
    items={[
        {
            children: 'Overview',
            slotContent: 'Overview content',
            value: 'overview'
        },
        { children: 'Details', slotContent: 'Details content', value: 'details' }
    ]}
/>
```

### With Links Tabs

Use when tabs drive client-side navigation. Adding `href` to items renders the root as a `<nav>`; pass `renderLink` to swap the underlying anchor for a framework router component (Next.js `Link`, React Router `Link`, etc.).

```tsx
<Tabs
    items={[
        { children: 'Home', value: 'home', href: '/' },
        { children: 'About', value: 'about', href: '/about' },
        { children: 'Contact', value: 'contact', href: '/contact' }
    ]}
    // this renderLink is equivalent to the default behavior of the Tabs component
    renderLink={item => <a href={item.href}>{item.children}</a>}
/>
```

### With Slots Tabs

Use `slotStart` / `slotEnd` to render labels, badges, or action buttons flanking the triggers inside the tabs list.

```tsx
<Tabs
    items={[
        { children: 'Tab 1', slotContent: 'Content 1', value: '1' },
        { children: 'Tab 2', slotContent: 'Content 2', value: '2' }
    ]}
    slotStart={<span className="px-2 font-bold">Section</span>}
    slotEnd={
        <button type="button" className="ml-auto">
            + Add
        </button>
    }
/>
```

### Controlled Tabs

Use controlled mode when tab content lives outside the `Tabs` component — e.g. when content is large or shared with other UI. Pass `value` + `onValueChange` and render the panels yourself instead of using `slotContent` per item.

```tsx
;() => {
    const [value, setValue] = useState('account')
    return (
        <Flex direction="col" gap="3">
            <Tabs
                items={[
                    { children: 'Account', value: 'account' },
                    { children: 'Notifications', value: 'notifications' },
                    { children: 'Billing', value: 'billing' }
                ]}
                value={value}
                onValueChange={setValue}
            />
            {value === 'account' && <Text>Account settings content</Text>}
            {value === 'notifications' && <Text>Notification preferences content</Text>}
            {value === 'billing' && <Text>Billing details content</Text>}
        </Flex>
    )
}
```

### Composed

```tsx
<TabsRoot defaultValue="tab1">
    <TabsList>
        <TabsTrigger value="tab1">Tab 1</TabsTrigger>
        <TabsTrigger value="tab2">Tab 2</TabsTrigger>
    </TabsList>
    <TabsContent value="tab1">Content for tab 1</TabsContent>
    <TabsContent value="tab2">Content for tab 2</TabsContent>
</TabsRoot>
```

## Props

| Prop                | Type                                                                                                                           | Default           | Description                                                                                                                                                                     |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------ | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **items** \*        | `{ asChild: boolean; children: ReactNode; slotContent: ReactNode; value: string; disabled: boolean; href: string }[]`          | -                 | List of tabs to render                                                                                                                                                          |
| activationMode      | `"manual" \| "automatic"`                                                                                                      | `"manual"`        | Whether tabs activate automatically when they receive focus or require Enter/Space.                                                                                             |
| defaultValue        | `string`                                                                                                                       | `items[0]?.value` | The value of the tabs when initially rendered. Use when you do not need to control the state of the tabs.                                                                       |
| hideOverflowButtons | `boolean`                                                                                                                      | `false`           | Hide the overflow scroll buttons                                                                                                                                                |
| kind                | `"primary" \| "secondary" \| "tertiary"`                                                                                       | `"primary"`       | Visual style variant of the tabs list                                                                                                                                           |
| onValueChange       | `(value: string) => void`                                                                                                      | -                 | Event handler called when the value changes                                                                                                                                     |
| ref                 | `((instance: HTMLDivElement) => void \| (() => void \| { [UNDEFINED_VOID_ONLY]: never; })) \| React.RefObject<HTMLDivElement>` | -                 | Allows getting a ref to the component instance. Once the component unmounts, React will set `ref.current` to `null` (or call the ref with `null` if you passed a callback ref). |
| renderLink          | `undefined \| (item: TabItem & { href: string; }) => ReactNode`                                                                | -                 | Render a custom link component for navigation mode. When provided, the root element will be a <nav>. When omitted, the root element will be a <div>.                            |
| slotEnd             | `ReactNode`                                                                                                                    | -                 | Content to display at the end of the tabs list.                                                                                                                                 |
| slotStart           | `ReactNode`                                                                                                                    | -                 | Content to display at the start of the tabs list.                                                                                                                               |
| value               | `string`                                                                                                                       | -                 | The controlled value of the tabs. Must be used in conjunction with `onValueChange`.                                                                                             |
| visibleRange        | `number[]`                                                                                                                     | -                 | A range of indices to display, with ellipses for gaps. For example, [1,2,3,8,9,10] would show items 1-3, ellipsis, then 8-10.                                                   |

`* = required prop`
