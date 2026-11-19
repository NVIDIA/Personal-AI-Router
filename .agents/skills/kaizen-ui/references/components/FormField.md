<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# FormField

Also known as: Form Group, Fieldset, Form Input

A component for displaying a form field with label, description, helper text, and validation messages.
FormField automatically adapts to different container widths. For horizontal layouts (`labelPosition="left"`),
it will switch to vertical layout on small screens.

## Examples

### Basic Form Field

```tsx
<FormField slotLabel="Email Address" slotHelp="We will not share your email.">
    <TextInput
        type="email"
        pattern="^[^\s@]+@[^\s@]+\.[^\s@]+$"
        placeholder="name@example.com"
        required
    />
</FormField>
```

### Text Area Form Field

```tsx
<FormField slotLabel="Description" slotHelp="Your description will be used to generate a report.">
    <TextArea placeholder="Enter your description" required />
</FormField>
```

### Select Form Field

```tsx
<FormField
    slotLabel="Preferred Contact Method"
    slotHelp="Your preferred contact method for communication."
>
    <Select items={['Phone', 'Email', 'Telegram', 'WhatsApp']} />
</FormField>
```

### Checkbox Form Field

```tsx
<FormField slotLabel="Accept Terms" slotHelp="I agree to the terms and conditions.">
    <Checkbox />
</FormField>
```

### Switch Form Field

```tsx
<FormField
    slotLabel="Enable Notifications"
    slotHelp="You'll receive email notifications for important updates."
>
    <Switch />
</FormField>
```

### Upload Form Field

```tsx
<Upload
    renderInput={inputNode => (
        <FormField
            name="documents"
            slotLabel="Upload Documents"
            slotHelp="Files will be deleted after 30 days."
        >
            {inputNode}
        </FormField>
    )}
    required
/>
```

### Error Form Field

Use when the field fails validation and the user needs guidance to correct it.

```tsx
<FormField slotLabel="Email Address" slotError="Please enter a valid email address." status="error">
    <TextInput placeholder="name@example.com" />
</FormField>
```

### Success Form Field

Use to confirm the field value has been validated successfully.

```tsx
<FormField slotLabel="Email Address" slotSuccess="Looks good!" status="success">
    <TextInput placeholder="name@example.com" />
</FormField>
```

### Required Form Field

When set, renders an asterisk next to the label. If the wrapped input has `:required`, the form field will automatically render the required indicator.

```tsx
<Flex>
    <FormField slotLabel="Email Address">
        <TextInput
            type="email"
            // :required is true so FormField will render the required indicator
            // (preferred over setting required on FormField)
            required
            placeholder="name@example.com"
        />
    </FormField>
    <FormField
        slotLabel="Full Name"
        // sets required on KUI form controls rendered within
        required
    >
        <TextInput placeholder="Jane Doe" />
    </FormField>
</Flex>
```

### With Info Form Field

Use when the field label alone is insufficient and users benefit from an info popover.

```tsx
<FormField
    slotLabel="API Key"
    slotInfo="Your API key is used to authenticate requests."
    slotHelp="Found in your account settings."
>
    <TextInput placeholder="sk-..." />
</FormField>
```

### Left Label Form Field

Top-aligned labels(default) enhance clarity, provide space for longer labels, and improve scannability on various screen sizes. Left-aligned labels should be used for dense forms to save space or for multi-column layouts. Note that left aligned labels will automatically switch to vertical layout if there is insufficient space.

```tsx
<FormField labelPosition="left" slotLabel="Username" slotHelp="Choose a unique username.">
    <TextInput placeholder="jdoe" />
</FormField>
```

### Composed

Use composed primitives when you need full control over the form field layout and context.

```tsx
<FormFieldRoot
    context={{
        id: 'demo-field',
        status: 'error',
        name: 'demo-field',
        required: true,
        'aria-labelledby': 'demo-field-label',
        'aria-describedby': 'demo-field-helper'
    }}
>
    <FormFieldContentGroup>
        <FormFieldLabelGroup>
            <label htmlFor="demo-field" id="demo-field-label">
                Label
            </label>
        </FormFieldLabelGroup>
        <TextInput placeholder="Composed field" />
    </FormFieldContentGroup>
    <FormFieldHelper id="demo-field-helper">Helper text for the field.</FormFieldHelper>
</FormFieldRoot>
```

## Props

| Prop          | Type                                                                                                                                                                                                                                                                                                                                                                                                                                                   | Default | Description                                                                                                                                                                                                                                                                              |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild       | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                              | -       | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                            |
| children      | `string \| number \| bigint \| false \| true \| React.ReactElement<unknown, string \| React.JSXElementConstructor<any>> \| Iterable<React.ReactNode> \| React.ReactPortal \| Promise<AwaitedReactNode> \| ((args: { id: string; name: string; status: "error" \| "success"; aria-describedby: string; aria-labelledby: string; aria-details: string; required: boolean }) => React.ReactElement<unknown, string \| React.JSXElementConstructor<any>>)` | -       | The children of the form field. Can be a render function that receives form field context.                                                                                                                                                                                               |
| id            | `string`                                                                                                                                                                                                                                                                                                                                                                                                                                               | -       | The ID of the form field (for connecting label and input). The value of this attribute must be unique. If not provided, a unique ID will be generated for you.                                                                                                                           |
| labelPosition | `"left" \| "top"`                                                                                                                                                                                                                                                                                                                                                                                                                                      | `"top"` | The position for the form field label and (optional) info icon.                                                                                                                                                                                                                          |
| name          | `string`                                                                                                                                                                                                                                                                                                                                                                                                                                               | -       | Name of the element. Used to identify fields in form submits.                                                                                                                                                                                                                            |
| required      | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                              | -       | When true, indicates that the user is required to fill out this field. Used to determine whether or not the asterisk is shown next to the label. By default, this will automatically determine if the form field contains a `:required` input, otherwise it will be manually controlled. |
| slotError     | `ReactNode`                                                                                                                                                                                                                                                                                                                                                                                                                                            | -       | The content to render as the error message. Visible when status is `"error"`.                                                                                                                                                                                                            |
| slotHelp      | `ReactNode`                                                                                                                                                                                                                                                                                                                                                                                                                                            | -       | The content to render as the help message (visible when status is not `"error"` or `"success"`).                                                                                                                                                                                         |
| slotInfo      | `ReactNode`                                                                                                                                                                                                                                                                                                                                                                                                                                            | -       | The content to render inside of the info icon popover.                                                                                                                                                                                                                                   |
| slotLabel     | `ReactNode`                                                                                                                                                                                                                                                                                                                                                                                                                                            | -       | The content to render as the label. This is required for accessibility. It's recommended                                                                                                                                                                                                 |
| slotSuccess   | `ReactNode`                                                                                                                                                                                                                                                                                                                                                                                                                                            | -       | The content to render as the success message. Visible when status is `"success"`.                                                                                                                                                                                                        |
| status        | `"error" \| "success"`                                                                                                                                                                                                                                                                                                                                                                                                                                 | -       | The status of the form field. Determines the visual state of the field.                                                                                                                                                                                                                  |
