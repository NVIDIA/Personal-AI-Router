<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Select

A Select is a dropdown that allows the user to select a value from a list of options.

## Examples

### Basic Select

```tsx
<Flex direction="col" gap="density-md">
    <Select
        items={['Apple', 'Banana', 'Cherry']}
        defaultValue="Banana"
        placeholder="Choose a fruit"
    />
    <Select
        items={['Apple', 'Banana', 'Cherry']}
        defaultValue={['Apple', 'Banana']}
        multiple
        placeholder="Choose multiple fruits"
    />
</Flex>
```

### With Object Items Select

Use object items when the stored value should differ from the visible label — for example, when the value is an ID and the label is human-readable.

```tsx
<Select
    items={[
        { value: '1', children: 'Apple' },
        { value: '2', children: 'Banana' },
        { value: '3', children: 'Cherry' }
    ]}
    placeholder="Choose a fruit"
/>
```

### Rich Item Content Select

Pass `slotStart` / `slotEnd` on an item for leading or trailing icons, or use `children` directly for richer multi-line content (e.g. an icon paired with a primary label and a secondary metadata line). Set `filterValue` whenever `children` is non-string so type-ahead still works.

```tsx
<Select
    items={[
        {
            value: 'edit',
            children: 'Edit',
            slotStart: <Document />
        },
        {
            value: 'schedule',
            children: 'Schedule',
            slotStart: <Calendar />
        },
        {
            value: 'report-q1',
            filterValue: 'Q1 Report.pdf',
            children: (
                <Flex direction="col">
                    <Flex gap="1" align="center">
                        <DocumentLine />
                        Q1 Report.pdf
                    </Flex>
                    <Text kind="label/regular/sm">Updated 2 days ago</Text>
                </Flex>
            )
        }
    ]}
    placeholder="Choose an option"
/>
```

### With Trigger Slots Select

Use slotStart and slotEnd on the Select itself to add leading icons (e.g. a search glyph) or trailing affordances (e.g. an info tooltip) to the trigger.

```tsx
<Select
    items={['Apple', 'Banana', 'Cherry']}
    placeholder="Filter fruit"
    slotStart={<Filter />}
    slotEnd={<InfoCircle />}
/>
```

### Custom Render Value Select

Use renderValue when the trigger needs richer or differently formatted content than the matching item's children.

```tsx
<Select
    items={['Apple', 'Banana', 'Cherry']}
    defaultValue="Apple"
    renderValue={value => (value ? `Selected: ${value}` : 'No value selected')}
/>
```

### Grouped Items Select

Use when options logically belong to categories that help users scan the list.

```tsx
<Select
    items={[
        { slotHeading: 'Fruits', items: ['Apple', 'Banana'] },
        { slotHeading: 'Vegetables', items: ['Carrot', 'Lettuce'] }
    ]}
    placeholder="Choose an item"
/>
```

### Multiple Select

Use when users need to choose more than one option from the list.

```tsx
<Select items={['Red', 'Green', 'Blue']} multiple placeholder="Pick colors" />
```

### Multi Tag Select

Pair multiple + allowBackspaceRemoval with a tag-based renderValue when each selected value should appear as a clearly removable chip. The Backspace shortcut is opt-in because the default `N selected` summary has no per-item target.

```tsx
<Select
    allowBackspaceRemoval
    multiple
    defaultValue={['Apple', 'Banana']}
    items={['Apple', 'Banana', 'Cherry', 'Orange', 'Pineapple']}
    renderValue={(value, setValue) =>
        value.length > 0 &&
        value.map(v => (
            <Tag
                key={v}
                color="gray"
                density="compact"
                onClick={e => {
                    e.stopPropagation()
                    setValue(value.filter(p => p !== v))
                }}
            >
                {v}
                <Close />
            </Tag>
        ))
    }
/>
```

### Dismissible Select

Use when users need a quick way to clear their selection.

```tsx
<Select items={['Apple', 'Banana', 'Cherry']} dismissible defaultValue="Apple" />
```

### Floating Trigger Select

Use triggerKind='floating' when the Select sits on top of dense content (e.g. a toolbar) and should not render the default input border or background.

```tsx
<Select items={['Apple', 'Banana', 'Cherry']} defaultValue="Banana" triggerKind="floating" />
```

### Size Select

Match the trigger size to surrounding controls. The menu density derives from size by default; override it with the density prop when needed.

```tsx
<Flex direction="col" gap="2">
    <Select size="small" items={['Apple', 'Banana']} placeholder="Small" />
    <Select size="medium" items={['Apple', 'Banana']} placeholder="Medium" />
    <Select size="large" items={['Apple', 'Banana']} placeholder="Large" />
</Flex>
```

### Disabled Select

Use when the selection is locked by external conditions.

```tsx
<Select items={['Apple', 'Banana']} disabled defaultValue="Apple" />
```

### Read Only Select

Use readOnly (instead of disabled) when the value should remain visible and copyable but the user cannot open the menu or change the selection — common in review or summary screens.

```tsx
<Select items={['Apple', 'Banana']} readOnly defaultValue="Apple" />
```

### Error Status Select

Use to indicate the selection has failed validation.

```tsx
<Select items={['Apple', 'Banana']} status="error" placeholder="Required" />
```

### With Form Field Select

Wrap Select in FormField to attach a label, helper text, and validation status that stay in sync with the trigger via context.

```tsx
<FormField slotLabel="Contact Method" slotHelp="How would you like us to reach you?">
    <Select items={['Phone', 'Email', 'Both']} placeholder="Select" />
</FormField>
```

### In Form Select

Pass a name prop so the Select renders a hidden native <select> that participates in standard form submission — works without JavaScript.

```tsx
<form>
    <Flex direction="col" gap="density-md">
        <Select name="fruit" items={['Apple', 'Banana', 'Cherry']} placeholder="Pick a fruit" />
        <Button type="submit">Submit</Button>
    </Flex>
</form>
```

### Controlled Select

Use controlled mode when the selection state lives outside the Select — for example, when syncing with form libraries, URL state, or another component.

```tsx
;() => {
    const [value, setValue] = useState('Apple')
    return <Select items={['Apple', 'Banana', 'Cherry']} value={value} onValueChange={setValue} />
}
```

### Composed

Use the composed primitives when you need full control over trigger rendering, content layout, or custom items.

```tsx
<SelectRoot defaultValue="b">
    <SelectTrigger placeholder="Pick one" />
    <SelectContent>
        <SelectItem value="a">Option A</SelectItem>
        <SelectItem value="b">Option B</SelectItem>
        <SelectItem value="c">Option C</SelectItem>
    </SelectContent>
</SelectRoot>
```

## Props

| Prop                  | Type                                                                                                                                                                                                                                                                                                                                                                                                                                                                                | Default      | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **items** \*          | `string \| ({ children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; value: string; danger: boolean; slotEnd: ReactNode; slotStart: ReactNode; filterValue: string }) \| ({ items: string \| ({ children: ReactNode; onSelect: (event: Event) => void; disabled: boolean; value: string; danger: boolean; slotEnd: ReactNode; slotStart: ReactNode; filterValue: string })[]; children: never; kind: "section"; filterValue: never; slotHeading: ReactNode })[]` | -            | The items to render in the Select.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| allowBackspaceRemoval | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | `false`      | In multi-select mode, pressing `Backspace` while the trigger button is focused removes the last selected value. Defaults to `false` because the default Select trigger shows a `"N item(s) selected"` summary with no per-item control to indicate which value would be removed. Opt in when you have provided a `renderValue` (or other display) that makes the target selection visually obvious.                                                                                                                                                                                      |
| autoFocusOnHide       | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | `true`       | Whether the Select trigger should be focused on hide.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| defaultOpen           | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | -            | The open state of the Select when it is initially rendered. Use when you do not need to control its open state.                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| defaultValue          | `string \| string[] & string \| string[] \| string & string[]`                                                                                                                                                                                                                                                                                                                                                                                                                      | -            | The value of the select when initially rendered. Use when you do not need to control the state of the select. Use a string for single value selects and an array of strings for multiple value selects.                                                                                                                                                                                                                                                                                                                                                                                  |
| density               | `"compact" \| "standard" \| "spacious"`                                                                                                                                                                                                                                                                                                                                                                                                                                             | `"standard"` | The "density" of the component. This affects the padding/spacing of the component and its children. By default, the component will inherit density from its parent, which is usually `standard`. - Setting to `null` or `undefined` will allow the component to inherit density from its parent. - Setting to `compact` will reduce the padding/spacing of the component and its children. - Setting to `standard` will use the standard padding/spacing of the component and its children. - Setting to `spacious` will increase the padding/spacing of the component and its children. |
| disabled              | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | -            | The disabled state of the Select.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| dismissible           | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | -            | When true, renders a dismiss button that will clear the select                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| hideOnEscape          | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | `true`       | Whether the Select content should be hidden when the escape key is pressed.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| multiple              | `false \| true`                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | -            | When `mutiple` is true the Select allows for multiple values to be selected.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| name                  | `string`                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | -            | The name of the Select. When provided, the native fallback select participates in form submission.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| onKeyDown             | `(event: React.KeyboardEvent<HTMLButtonElement>) => void`                                                                                                                                                                                                                                                                                                                                                                                                                           | -            | Keyboard event handler invoked on the interactive trigger button. Called before the Select's built-in shortcuts (ArrowDown/ArrowUp open and, when `allowBackspaceRemoval` is enabled, Backspace removal); call `event.preventDefault()` to cancel those shortcuts.                                                                                                                                                                                                                                                                                                                       |
| onOpenChange          | `(open: boolean) => void`                                                                                                                                                                                                                                                                                                                                                                                                                                                           | -            | The callback to be called when the Select is opened or closed.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| onScrollToBottom      | `() => void`                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | -            | Callback function that is called when the menu is scrolled to the bottom. Useful for loading more items.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| onValueChange         | `(value: string) => void \| (value: string[]) => void`                                                                                                                                                                                                                                                                                                                                                                                                                              | -            | Callback when the selected value of the select changes.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| open                  | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | -            | The controlled open state of the Select                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| placeholder           | `ReactNode`                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | -            | Placeholder text for the Select trigger                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| readOnly              | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | -            | Whether the Select is read-only. A read-only Select will not allow the user to change the selected value or view the menu.                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| renderValue           | `(value: string, setValue: (value: string \| ((prev: string) => string)) => void) => ReactNode \| (value: string[], setValue: (value: string[] \| ((prev: string[]) => string[])) => void) => ReactNode`                                                                                                                                                                                                                                                                            | -            | Controls the rendering of the selected select value in the trigger. By default the select will render the children of the selected item for a single value select. For a multiple value select, the select renders the count of selected items.                                                                                                                                                                                                                                                                                                                                          |
| required              | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | -            | Whether the Select is required                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| selectRef             | `((instance: HTMLSelectElement) => void \| (() => void \| { [UNDEFINED_VOID_ONLY]: never; })) \| React.RefObject<HTMLSelectElement>`                                                                                                                                                                                                                                                                                                                                                | -            | A ref to the internal native `<select>` element used for form integration. Only populated when the `name` prop is provided.                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| side                  | `"right" \| "bottom" \| "left" \| "top"`                                                                                                                                                                                                                                                                                                                                                                                                                                            | `"bottom"`   | The side the Select menu will be positioned.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| size                  | `"small" \| "medium" \| "large"`                                                                                                                                                                                                                                                                                                                                                                                                                                                    | `"medium"`   | The size of the select trigger. By default this will also control the `density` of the `SelectContent` - to override this, use the `density` prop.                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| slotEnd               | `ReactNode`                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | -            | Content to render on the end side of the Select trigger.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| slotStart             | `ReactNode`                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | -            | Content to render on the start side of the Select trigger.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| status                | `"error" \| "success"`                                                                                                                                                                                                                                                                                                                                                                                                                                                              | -            | The status of the input. Use `withValidation` to automatically apply success/error states based on `:user-valid` and `:user-invalid` pseudo classes.                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| triggerKind           | `"flat" \| "floating"`                                                                                                                                                                                                                                                                                                                                                                                                                                                              | `"flat"`     | The kind of the Select trigger. Used to determine whether or not to render background colors and borders.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| value                 | `string \| string[] & string \| string[] \| string & string[]`                                                                                                                                                                                                                                                                                                                                                                                                                      | -            | The controlled value of the select. Must be used in conjunction with `onValueChange`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

`* = required prop`
