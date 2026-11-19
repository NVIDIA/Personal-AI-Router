<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Menu

A static menu component that displays a list of items.

Note: if you're looking for a menu that can be used as a dropdown, see the `Dropdown` component.
This is the menu component itself and does not have behaviour to hide/show itself.

## Examples

### Basic Menu

Use when you need a static, always-visible list of selectable actions or navigation items.

```tsx
<Menu
    items={[
        {
            children: 'Dashboard'
            // onSelect: handleSelect
        },
        {
            children: 'Settings'
            // onSelect: handleSelect
        },
        {
            children: 'Help'
            // onSelect: handleSelect
        }
    ]}
/>
```

### With Icons Menu

Use when menu items benefit from leading icons to improve scannability and recognition.

```tsx
<Menu
    items={[
        { children: 'Profile', slotStart: <Icon />, onSelect: handleSelect },
        {
            children: 'Notifications',
            slotStart: <Icon />,
            onSelect: handleSelect
        },
        {
            children: 'Logout',
            slotStart: <Icon />,
            danger: true,
            onSelect: handleSelect
        }
    ]}
/>
```

### With Sections Menu

Use when items belong to distinct categories that benefit from labeled group headings.

```tsx
<Menu
    items={[
        {
            slotHeading: 'File',
            items: [
                { children: 'New', onSelect: handleSelect },
                { children: 'Open', onSelect: handleSelect },
                { children: 'Save', disabled: true, onSelect: handleSelect }
            ]
        },
        {
            slotHeading: 'Edit',
            items: [
                { children: 'Undo', onSelect: handleSelect },
                { children: 'Redo', onSelect: handleSelect },
                { children: 'Delete', danger: true, onSelect: handleSelect }
            ]
        }
    ]}
/>
```

### With Dividers Menu

Use divider entries to visually separate unrelated groups of items without adding a section heading.

```tsx
<Menu
    items={[
        { children: 'Edit Profile', onSelect: handleSelect },
        { children: 'View Settings', onSelect: handleSelect },
        { kind: 'divider' },
        { children: 'Help & Support', onSelect: handleSelect },
        { kind: 'divider' },
        { children: 'Sign Out', danger: true, onSelect: handleSelect }
    ]}
/>
```

### Checkbox Menu

Use when the menu allows toggling multiple independent boolean options.

```tsx
<Menu
    items={[
        {
            children: 'Auto-save',
            kind: 'checkbox',
            defaultChecked: true
        },
        {
            children: 'Spell Check',
            kind: 'checkbox'
        },
        {
            children: 'Word Wrap',
            kind: 'checkbox'
        }
    ]}
/>
```

### Radio Menu

Use when the user must select exactly one option from a mutually exclusive group within the menu.

```tsx
<Menu
    items={[
        {
            kind: 'radio',
            name: 'sort',
            slotHeading: 'Sort By',
            defaultValue: 'name',
            items: [
                { children: 'Name', value: 'name' },
                { children: 'Date', value: 'date' },
                { children: 'Size', value: 'size' }
            ]
        }
    ]}
/>
```

### Filterable Menu

Use when the menu has many items and the user benefits from filtering by typing.

```tsx
<Menu
    filterable
    items={[
        { children: 'Apple', onSelect: handleSelect },
        { children: 'Banana', onSelect: handleSelect },
        { children: 'Cherry', onSelect: handleSelect },
        { children: 'Date', onSelect: handleSelect },
        { children: 'Elderberry', onSelect: handleSelect }
    ]}
/>
```

### With Empty State Menu

Use `data-empty-message` to customize the message shown when a filterable menu has no matching items.

```tsx
<Menu
    filterable
    defaultFilterValue="no matches"
    data-empty-message="No items match your search"
    items={['Edit Profile', 'View Settings', 'Sign Out']}
/>
```

### Mixed Items Menu

Use when a single menu needs to combine actions, toggleable settings, and a mutually exclusive selection — group items of the same kind together.

```tsx
<Menu
    items={[
        {
            slotHeading: 'Actions',
            items: [
                { children: 'Edit Profile', onSelect: handleSelect },
                { children: 'View Settings', onSelect: handleSelect }
            ]
        },
        {
            slotHeading: 'Preferences',
            items: [
                { kind: 'checkbox', children: 'Email Notifications' },
                { kind: 'checkbox', children: 'Push Notifications' }
            ]
        },
        {
            name: 'theme',
            kind: 'radio',
            defaultValue: 'system',
            slotHeading: 'Theme',
            items: [
                { children: 'System', value: 'system' },
                { children: 'Light', value: 'light' },
                { children: 'Dark', value: 'dark' }
            ]
        }
    ]}
/>
```

### With Form Actions Menu

Use `formAction` and `formMethod` inside a `<form>` to submit menu actions to different endpoints with no client-side JavaScript required.

```tsx
<form>
    <Menu
        items={[
            {
                children: 'Approve',
                formAction: '/api/actions/approve',
                formMethod: 'post'
            },
            {
                children: 'Reject',
                formAction: '/api/actions/reject',
                formMethod: 'post'
            },
            {
                children: 'Request Changes',
                formAction: '/api/actions/request-changes',
                formMethod: 'post'
            }
        ]}
    />
</form>
```

### Composed

Use composed primitives when you need full control over the menu layout, mixing sections with different item types.

```tsx
<MenuRoot>
    <MenuSection slotHeading="Actions">
        <MenuItem
        // onSelect={handleSelect}
        >
            New File
        </MenuItem>
        <MenuItem
        // onSelect={handleSelect}
        >
            Open File
        </MenuItem>
    </MenuSection>
    <MenuSection slotHeading="Preferences">
        <MenuCheckboxItem defaultChecked>Auto-save</MenuCheckboxItem>
        <MenuRadioGroup name="view" slotHeading="View Mode" defaultValue="grid">
            <MenuRadioGroupItem value="grid">Grid</MenuRadioGroupItem>
            <MenuRadioGroupItem value="list">List</MenuRadioGroupItem>
        </MenuRadioGroup>
    </MenuSection>
</MenuRoot>
```

## Props

| Prop                | Type                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                | Default      | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| ------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **items** \*        | `string \| ({ kind: "section"; slotHeading: ReactNode; children: never; items: ({ kind: "default"; filterValue: string; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; danger: boolean; slotEnd: ReactNode; slotStart: ReactNode; value: string; formAction: string; formMethod: "get" \| "post" }) \| ({ kind: "checkbox"; filterValue: string; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; danger: boolean; slotEnd: ReactNode; slotStart: ReactNode; defaultChecked: boolean; checked: false \| true \| "indeterminate"; error: boolean; onCheckedChange: (checked: false \| true \| "indeterminate") => void; slotControl: ReactNode }) \| { kind: "divider"; width: "small" \| "medium" \| "large"; filterValue: never }[]; filterValue: never }) \| ({ kind: "default"; filterValue: string; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; danger: boolean; slotEnd: ReactNode; slotStart: ReactNode; value: string; formAction: string; formMethod: "get" \| "post" }) \| ({ kind: "checkbox"; filterValue: string; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; danger: boolean; slotEnd: ReactNode; slotStart: ReactNode; defaultChecked: boolean; checked: false \| true \| "indeterminate"; error: boolean; onCheckedChange: (checked: false \| true \| "indeterminate") => void; slotControl: ReactNode }) \| ({ name: string; kind: "radio"; radioKind: "radio" \| "check"; slotHeading: ReactNode; items: { kind: never; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; slotEnd: ReactNode; slotStart: ReactNode; filterValue: string; value: string; danger: boolean; required: boolean }[]; defaultValue: string; disabled: boolean; value: string; required: boolean; error: boolean; onValueChange: (value: string) => void }) \| ({ kind: never; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; slotEnd: ReactNode; slotStart: ReactNode; filterValue: string; value: string; danger: boolean; required: boolean }) \| { kind: "divider"; width: "small" \| "medium" \| "large"; filterValue: never }[]` | -            | The items to render in the menu.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| defaultFilterValue  | `string`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | -            | This sets the default value of the menu search input.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| density             | `"compact" \| "standard" \| "spacious"`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             | `"standard"` | The "density" of the component. This affects the padding/spacing of the component and its children. By default, the component will inherit density from its parent, which is usually `standard`. - Setting to `null` or `undefined` will allow the component to inherit density from its parent. - Setting to `compact` will reduce the padding/spacing of the component and its children. - Setting to `standard` will use the standard padding/spacing of the component and its children. - Setting to `spacious` will increase the padding/spacing of the component and its children. |
| filterable          | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | `false`      | Whether the menu is filterable.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| filterMatchFn       | `"disable" \| ((matchTerm: string, value: string) => boolean)`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | -            | The function to use to match items against the search value.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| filterValue         | `string`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | -            | The controlled value of the menu search input. Must be used in conjunction with `onFilterChange`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| onFilterChange      | `(value: string) => void`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | -            | Callback fired when the search value changes.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| onItemCheckedChange | `(item: { kind: "checkbox"; filterValue: string; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; danger: boolean; slotEnd: ReactNode; slotStart: ReactNode; defaultChecked: boolean; checked: false \| true \| "indeterminate"; error: boolean; onCheckedChange: (checked: false \| true \| "indeterminate") => void; slotControl: ReactNode }, checked: false \| true \| "indeterminate") => void`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       | -            | Callback fired when a menu item's checked state changes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| onItemSelect        | `(event: Event, item: ({ kind: "default"; filterValue: string; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; danger: boolean; slotEnd: ReactNode; slotStart: ReactNode; value: string; formAction: string; formMethod: "get" \| "post" }) \| ({ kind: "checkbox"; filterValue: string; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; danger: boolean; slotEnd: ReactNode; slotStart: ReactNode; defaultChecked: boolean; checked: false \| true \| "indeterminate"; error: boolean; onCheckedChange: (checked: false \| true \| "indeterminate") => void; slotControl: ReactNode }) \| ({ kind: never; children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; slotEnd: ReactNode; slotStart: ReactNode; filterValue: string; value: string; danger: boolean; required: boolean })) => void`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              | -            | Callback fired when a menu item is selected                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| ref                 | `React.RefObject<HTMLMenuElement> \| ((instance: HTMLMenuElement) => void \| (() => void \| { [UNDEFINED_VOID_ONLY]: never; }))`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    | -            |                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |

`* = required prop`
