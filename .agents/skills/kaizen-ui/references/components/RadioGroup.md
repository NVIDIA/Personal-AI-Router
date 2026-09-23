<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# RadioGroup

A set of radio buttons where no more than one of the options can be selected at a time.

**Progressive Enhancement:** Uses native `<input type="radio">` elements which provide
built-in keyboard navigation (arrow keys), mutual exclusion, and form submission without
JavaScript.

## Examples

### Basic Radio Group

```tsx
<RadioGroup
    name="color"
    defaultValue="blue"
    items={[
        { children: 'Red', value: 'red' },
        { children: 'Blue', value: 'blue' },
        { children: 'Green', value: 'green' }
    ]}
/>
```

### With Icons Radio Group

Use icons in `children` to give each option a visual anchor when the label alone is ambiguous.

```tsx
<RadioGroup
    name="backup-frequency"
    defaultValue="weekly"
    items={[
        {
            children: (
                <>
                    <CheckCircle />
                    Daily
                </>
            ),
            value: 'daily'
        },
        {
            children: (
                <>
                    <Clock />
                    Weekly
                </>
            ),
            value: 'weekly'
        },
        {
            children: (
                <>
                    <Calendar />
                    Monthly
                </>
            ),
            value: 'monthly'
        }
    ]}
/>
```

### Horizontal Radio Group

Use horizontal orientation when the option labels are short and fit on a single row.

```tsx
<RadioGroup
    name="size"
    defaultValue="medium"
    orientation="horizontal"
    items={[
        { children: 'Small', value: 'small' },
        { children: 'Medium', value: 'medium' },
        { children: 'Large', value: 'large' }
    ]}
/>
```

### With Danger Item Radio Group

Use the `danger` flag on an item to warn users about a destructive or irreversible choice.

```tsx
<RadioGroup
    name="action"
    defaultValue="keep"
    items={[
        { children: 'Keep data', value: 'keep' },
        { children: 'Archive data', value: 'archive' },
        { children: 'Delete permanently', value: 'delete', danger: true }
    ]}
/>
```

### Error Radio Group

Use the group-level `error` prop to surface form validation failures across every item — pair with a `FormField` to render the error message.

```tsx
<RadioGroup
    name="payment-method"
    error
    items={[
        { children: 'Credit Card', value: 'credit-card' },
        { children: 'Debit Card', value: 'debit-card' },
        { children: 'PayPal', value: 'paypal' }
    ]}
/>
```

### With Disabled Item Radio Group

Disable individual items with the per-item `disabled` flag when only some options are unavailable in the current context.

```tsx
<RadioGroup
    name="plan"
    defaultValue="basic"
    items={[
        { children: 'Basic', value: 'basic' },
        { children: 'Pro', value: 'pro' },
        { children: 'Enterprise', value: 'enterprise', disabled: true }
    ]}
/>
```

### Disabled Radio Group

Use when the selection is locked by external conditions and cannot be changed.

```tsx
<RadioGroup
    name="locked"
    defaultValue="on"
    disabled
    items={[
        { children: 'On', value: 'on' },
        { children: 'Off', value: 'off' }
    ]}
/>
```

### Label Start Radio Group

Use start label placement when end-aligning radio indicators improves visual scanning.

```tsx
<RadioGroup
    name="align"
    defaultValue="left"
    labelSide="start"
    items={[
        { children: 'Left', value: 'left' },
        { children: 'Center', value: 'center' },
        { children: 'Right', value: 'right' }
    ]}
/>
```

### Hidden Indicator Radio Group

Use hidden indicators with card-style items when the selection state is communicated by the card styling.

```tsx
<RadioGroup
    name="plan-tier"
    defaultValue="pro"
    showIndicator={false}
    items={[
        { children: 'Free', value: 'free' },
        { children: 'Pro', value: 'pro' },
        { children: 'Enterprise', value: 'enterprise' }
    ]}
/>
```

### Composed

Use the composed primitives when you need full control over item layout or custom content around each radio input.

```tsx
<RadioGroupRoot name="gpu" defaultValue="4090" orientation="horizontal">
    <RadioGroupItem>
        <RadioGroupInput value="4090" />
        RTX 4090
    </RadioGroupItem>
    <RadioGroupItem>
        <RadioGroupInput value="4080" />
        RTX 4080
    </RadioGroupItem>
    <RadioGroupItem>
        <RadioGroupInput value="3090" />
        RTX 3090
    </RadioGroupItem>
</RadioGroupRoot>
```

## Props

| Prop          | Type                                                                                                                                                                                                                                                                                                                                                                                                                                                             | Default      | Description                                                                                                                                                                          |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **items** \*  | `Pick<RadioGroupInputProps, "disabled" \| "value" \| "danger" \| "required"> & { children: React.ReactNode; attributes?: { RadioGroupItem?: NativeElementAttributes<"label", React.ForwardRefExoticComponent<Omit<RadioGroupItemProps, "ref"> & React.RefAttributes<HTMLLabelElement>>>; RadioGroupInput?: NativeElementAttributes<"input", React.ForwardRefExoticComponent<Omit<RadioGroupInputProps, "ref"> & React.RefAttributes<HTMLInputElement>>>; }; }[]` | -            | The items to render in the radio group.                                                                                                                                              |
| defaultValue  | `string`                                                                                                                                                                                                                                                                                                                                                                                                                                                         | -            | The value of the radio group when it is initially rendered. Use when you do not need to control the value of the radio group.                                                        |
| disabled      | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                        | -            | If true, the radio group will be disabled.                                                                                                                                           |
| error         | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                        | -            | If true, the radio group will show an error state                                                                                                                                    |
| labelSide     | `"start" \| "end"`                                                                                                                                                                                                                                                                                                                                                                                                                                               | `"end"`      | Which side of the radio input the label renders on.                                                                                                                                  |
| name          | `string`                                                                                                                                                                                                                                                                                                                                                                                                                                                         | -            | Shared HTML `name` for all radios in the group. When omitted, uses `name` from a wrapping `FormField` when present.                                                                  |
| onValueChange | `(value: string) => void`                                                                                                                                                                                                                                                                                                                                                                                                                                        | -            | Event handler called when the value of the radio group changes.                                                                                                                      |
| orientation   | `"horizontal" \| "vertical"`                                                                                                                                                                                                                                                                                                                                                                                                                                     | `"vertical"` | The orientation of the radio group. Determines the layout of the radio items.                                                                                                        |
| required      | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                        | -            | When true, indicates that the user must check a radio item before the owning form can be submitted.                                                                                  |
| showIndicator | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                                                        | `true`       | Whether to show the circular radio indicator on each item. When `false`, inputs stay in the DOM for accessibility and forms but are visually hidden—pair with card styling on items. |
| value         | `string`                                                                                                                                                                                                                                                                                                                                                                                                                                                         | -            | The controlled value of the radio group. Must be used in conjunction with `onValueChange`.                                                                                           |

`* = required prop`
