<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# TextArea

A textarea with additional capabilities such as icons and status.

## Examples

### Basic Text Area

```tsx
<TextArea defaultValue="Lorem ipsum dolor sit amet, consectetur adipiscing elit. Sed do eiusmod tempor incididunt ut labore et dolore magna aliqua." />
```

### Controlled Text Area

Controlled mode lets you manage value externally, useful for syncing with form libraries, character counters, or external validation.

```tsx
;() => {
    const [value, setValue] = useState('')
    return <TextArea value={value} onValueChange={setValue} placeholder="Type something" />
}
```

### With Placeholder Text Area

Use placeholder to hint at the expected format. Do not use it as a substitute for a label or required helper text.

```tsx
<TextArea placeholder="Enter a description..." />
```

### Auto Resize Text Area

Use when the textarea should grow with content up to a max height, avoiding scrollbars for short entries. Override `--max-auto-height` (default 400px) to change the cap.

```tsx
<TextArea resizeable="auto" placeholder="Start typing..." />
```

### Manual Resize Text Area

Use when the user should control the textarea height manually via a drag handle in the bottom-right corner.

```tsx
<TextArea resizeable="manual" placeholder="Drag to resize" />
```

### Size Text Area

Match the textarea size to surrounding form controls. Default is `medium`.

```tsx
<Flex direction="col" gap="density-md">
    <TextArea size="small" placeholder="Small" />
    <TextArea size="medium" placeholder="Medium" />
    <TextArea size="large" placeholder="Large" />
</Flex>
```

### Status Text Area

Set `status` to drive validation styling manually — `error` for failed validation (pair with helper text on the wrapping FormField) or `success` to confirm a passing value. Reach for `withValidation` when native HTML constraints can drive the state automatically.

```tsx
<Flex direction="col" gap="density-md">
    <TextArea status="error" defaultValue="Invalid content" />
    <TextArea status="success" defaultValue="Looks good" />
</Flex>
```

### With Validation Text Area

Set `withValidation` to drive status styling from the browser's native `:user-valid` / `:user-invalid` pseudo-classes — no JS state required. Combine with `required`, `minLength`, `maxLength`, etc.

```tsx
<TextArea withValidation required placeholder="Will turn red on blur if empty" />
```

### Disabled Text Area

Use when the field is unavailable due to external state (e.g. permissions). Prefer `readOnly` when the value should still be selectable.

```tsx
<TextArea disabled defaultValue="Read-only content" />
```

### Read Only Text Area

Use when the value should remain visible and copyable but cannot be edited — common in review or summary screens.

```tsx
<TextArea readOnly defaultValue="Read-only value" />
```

### With Slots Text Area

Use `slotStart` / `slotEnd` to attach affordances like icons or action buttons. Use `mb-auto` / `mt-auto` utilities to anchor slot content to the top or bottom of a multi-line field.

```tsx
<TextArea
    placeholder="Add a comment..."
    slotStart={<Document className="mb-auto" />}
    slotEnd={
        <Button className="mt-auto" color="brand" size="small" aria-label="Send">
            <ChevronRight />
        </Button>
    }
/>
```

### Vertical Layout Text Area

Use `layout="vertical"` to stack slot content above and below the textarea — ideal for chat composers, comment boxes, or toolbars that need full-width controls.

```tsx
<TextArea
    layout="vertical"
    placeholder="Compose your message..."
    slotStart={
        <Badge color="green" kind="solid">
            Active
        </Badge>
    }
    slotEnd={
        <Flex gap="2" className="w-full">
            <Button kind="tertiary" size="small" aria-label="Attach">
                <Document />
            </Button>
            <Button className="ml-auto" color="brand" size="small" aria-label="Send">
                <ChevronRight />
            </Button>
        </Flex>
    }
/>
```

### With Form Field Text Area

Wrap TextArea in FormField to attach a label, helper text, and validation status that stay in sync with the field via context.

```tsx
<FormField slotLabel="Description" slotHelp="Your description will be used to generate a report.">
    <TextArea placeholder="Enter your description" required />
</FormField>
```

### Composed

Use the composed primitives when you need full control over the shell and textarea element layout — for example, to mix the textarea with sibling buttons inside the same shell.

```tsx
<InputShell>
    <TextAreaElement defaultValue="Composed primitives" resizeable="auto" />
</InputShell>
```

## Props

| Prop                 | Type                                                                     | Default        | Description                                                                                                                                                                                                                                                                                                                                              |
| -------------------- | ------------------------------------------------------------------------ | -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild              | `boolean`                                                                | -              | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                                                                            |
| disableFocusRedirect | `boolean`                                                                | `false`        | When true, the input will not redirect focus to the input when clicked.                                                                                                                                                                                                                                                                                  |
| kind                 | `"flat" \| "floating"`                                                   | `"flat"`       | The kind of input to render. - "flat" - renders with a border and background - "floating" - renders borderless with no background                                                                                                                                                                                                                        |
| layout               | `"horizontal" \| "vertical"`                                             | `"horizontal"` | The layout direction for slotted content. - "horizontal" - children are laid out in a row (default) - "vertical" - children are laid out in a column with auto height and block padding                                                                                                                                                                  |
| onValueChange        | `(value: string, event: React.ChangeEvent<HTMLTextAreaElement>) => void` | -              | Convenience callback, provides the value as the first argument                                                                                                                                                                                                                                                                                           |
| resizeable           | `"auto" \| "manual"`                                                     | -              | Controls the resize behavior of the text area. - `manual`: The text area can be manually resized vertically. - `auto`: The text area will be automatically resized to fit the content to a max height of `--max-auto-height` (400px by default). Set `--max-auto-height` to control the max height. - `undefined`: The text area will not be resizeable. |
| size                 | `"small" \| "medium" \| "large"`                                         | `"medium"`     | The size of the input to render. Available sizes are "small", "medium", and "large".                                                                                                                                                                                                                                                                     |
| slotEnd              | `ReactNode`                                                              | -              | Content to render on the end side of the textarea. By default this is the right, and `layout="vertical"` will have this on the bottom.                                                                                                                                                                                                                   |
| slotStart            | `ReactNode`                                                              | -              | Content to render on the start side of the textarea. By default this is the left, and `layout="vertical"` will have this on the top.                                                                                                                                                                                                                     |
| status               | `"error" \| "success"`                                                   | -              | The status of the input. Use `withValidation` to automatically apply success/error states based on `:user-valid` and `:user-invalid` pseudo classes.                                                                                                                                                                                                     |
| value                | `string`                                                                 | -              | The value of the text area. Must be used in combination with `onChange/onValueChange` to ensure the value can be updated, otherwise this becomes read only.                                                                                                                                                                                              |
| withValidation       | `boolean`                                                                | -              | When true, the input will automatically display success/error state based on `:user-valid` and `:user-invalid` styles                                                                                                                                                                                                                                    |
