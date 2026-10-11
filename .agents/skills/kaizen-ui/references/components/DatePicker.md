<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# DatePicker

A complete date picker component with input field(s) and calendar popup.
Supports both single date selection and date range selection modes.
The component handles manual date entry through input fields and provides a calendar interface for visual date selection.

## Examples

### Basic Date Picker

Use when users need to select a single date.

```tsx
<DatePicker />
```

### Range Date Picker

Set `kind="range"` when users need to select a start and end date, such as for filtering or scheduling.

```tsx
<DatePicker kind="range" />
```

### With Default Value Date Picker

Single pickers accept a `Date` for `defaultValue` / `value` and emit `Date | undefined` from `onValueChange`. Range pickers accept a `DateRange` object with optional `from` / `to` Date properties and emit `DateRange | undefined`. The value can become `undefined` when the user clears the field, so always treat the controlled value as nullable.

```tsx
<Flex direction="col" gap="2">
    <DatePicker defaultValue={new Date()} />
    <DatePicker
        kind="range"
        defaultValue={{
            from: new Date(),
            to: new Date(Date.now() + 7 * 86_400_000)
        }}
    />
</Flex>
```

### With Range Placeholder Date Picker

Pass an object `{ from, to }` to give the range fields distinct placeholders. A plain string is applied to both inputs.

```tsx
<DatePicker kind="range" placeholder={{ from: 'Start date', to: 'End date' }} />
```

### Disable Weekends Date Picker

Use `{ dayOfWeek: [...] }` to disable specific days, where 0 is Sunday and 6 is Saturday. The same matcher shape applies to range pickers.

```tsx
<DatePicker disabledMatcher={{ dayOfWeek: [0, 6] }} />
```

### Disable Past Dates Date Picker

Use `{ before }` to forbid dates earlier than a cutoff (here, today), or `{ after }` for the inverse.

```tsx
<DatePicker disabledMatcher={{ before: new Date() }} />
```

### Combined Disabled Matcher Date Picker

Pass an array to combine matchers; any date matched by _any_ entry is disabled. Other matcher shapes include a single `Date`, an array of `Date`s, a `{ from, to }` range, and a predicate `(date) => boolean`.

```tsx
<DatePicker kind="range" disabledMatcher={[{ dayOfWeek: [0, 6] }, { before: new Date() }]} />
```

### Constrained Range Date Picker

On a range picker `min` and `max` are the minimum and maximum number of **nights** in the selection — not absolute dates. HTML cannot couple two native date inputs, so these are only enforced by the enhanced widget; re-validate no-JS submissions with `validateDateRangeNights` on the server.

```tsx
<DatePicker kind="range" min={3} max={30} />
```

### Exclude Disabled Dates Range Date Picker

Range-only. With `excludeDisabledDates`, users cannot select a range that spans a disabled date. Without it, the range may straddle disabled dates as long as the endpoints themselves are selectable.

```tsx
<DatePicker kind="range" disabledMatcher={{ dayOfWeek: [0, 6] }} excludeDisabledDates />
```

### Calendar Bounds Date Picker

Use `startMonth` and `endMonth` to constrain the navigable calendar window (defaults: 100 years ago to 5 years from now). These values are also translated into native `min` / `max` for the no-JS fallback. Use `defaultMonth` to set the initially-visible month without selecting a date.

```tsx
<DatePicker
    startMonth={new Date(2024, 0, 1)}
    endMonth={new Date(2024, 11, 31)}
    defaultMonth={new Date(2024, 5, 1)}
/>
```

### Custom Format Date Picker

`format` accepts a `date-fns` token string (e.g. `"yyyy-MM-dd"`, `"dd-MM-yyyy"`, `"MMM d, yyyy"`). Display formatting only — the form value is always submitted as ISO `yyyy-MM-dd`.

```tsx
<DatePicker format="yyyy-MM-dd" />
```

### Custom Format Function Date Picker

Use `formatFn` for full control over display formatting; it overrides `format`. Always set `placeholder` when supplying `formatFn` because the format string can no longer be used as a default hint.

```tsx
<DatePicker
    formatFn={date =>
        date.toLocaleDateString('en-US', {
            month: 'short',
            day: 'numeric',
            year: 'numeric'
        })
    }
    placeholder="Mon D, YYYY"
/>
```

### Time Zone Date Picker

Pass an IANA name (e.g. `"America/New_York"`) or UTC offset (e.g. `"-05:00"`) so the picker interprets and renders the date in that zone instead of the user's local zone.

```tsx
<DatePicker timeZone="America/New_York" defaultValue={new Date('2024-06-01T12:00:00Z')} />
```

### Form Submission Date Picker

Apply the form `name` to `DatePickerNativeFallback`, not to `DatePickerInput`. The enhanced text input is display-only; the hidden native `<input type="date">` is the element that participates in form submission and always submits an ISO `yyyy-MM-dd` value. Use `parseDateFromFormData(formData, "dueDate")` on the server.

```tsx
<DatePicker
    attributes={{
        DatePickerNativeFallback: { name: 'dueDate' }
    }}
/>
```

### Form Submission Range Date Picker

Range pickers submit two independent ISO values, so set the `name` on both `DatePickerNativeFallbackFrom` and `DatePickerNativeFallbackTo`. Parse with `parseDateRangeFromFormData(formData, { from: "rolloutFrom", to: "rolloutTo" })`.

```tsx
<DatePicker
    kind="range"
    attributes={{
        DatePickerNativeFallbackFrom: { name: 'rolloutFrom' },
        DatePickerNativeFallbackTo: { name: 'rolloutTo' }
    }}
/>
```

### Custom Input Attributes Date Picker

Use the `attributes` API to set per-input `aria-label`, `autoComplete`, `data-*`, etc. without dropping to composed primitives. Single pickers expose `DatePickerInput`; range pickers expose `DatePickerInputFrom` and `DatePickerInputTo`.

```tsx
<DatePicker
    kind="range"
    attributes={{
        DatePickerInputFrom: {
            autoComplete: 'off',
            'aria-label': 'Trip start date'
        },
        DatePickerInputTo: {
            autoComplete: 'off',
            'aria-label': 'Trip end date'
        }
    }}
/>
```

### Composed

Use the composed primitives when you need full control over trigger, input, calendar layout, or to slot in a custom rendering between any of them.

```tsx
<DatePickerRoot kind="single">
    <DatePickerTrigger>
        <DatePickerInput />
        <DatePickerNativeFallback />
    </DatePickerTrigger>
    <DatePickerContent>
        <DatePickerCalendar />
    </DatePickerContent>
</DatePickerRoot>
```

## Props

| Prop                 | Type                                                                                                                                                                                                                                                                                                                                                                                                                             | Default            | Description                                                                                                                                                                                                                                    |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| align                | `"center" \| "start" \| "end"`                                                                                                                                                                                                                                                                                                                                                                                                   | `"center"`         | The alignment of the popover content relative to the trigger.                                                                                                                                                                                  |
| closeOnApply         | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                        | `true`             | Whether to close the datepicker when the apply button is clicked.                                                                                                                                                                              |
| defaultMonth         | `Date`                                                                                                                                                                                                                                                                                                                                                                                                                           | `Current month`    | The visible month in the datepicker calendar when initially rendered. Use when you do not need to control the month of the datepicker calendar.                                                                                                |
| defaultOpen          | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                        | `false`            | The open state of the datepicker when it is initially rendered. Use when you do not need to control its open state.                                                                                                                            |
| defaultValue         | `Date \| { from: Date; to?: Date; }`                                                                                                                                                                                                                                                                                                                                                                                             | -                  | The initial value.                                                                                                                                                                                                                             |
| disabled             | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                        | -                  | Whether the datepicker is disabled.                                                                                                                                                                                                            |
| disabledMatcher      | `false \| true \| Date \| ((date: Date) => boolean) \| Date[] \| { from: Date; to?: Date; } \| { before: Date; } \| { after: Date; } \| { before: Date; after: Date; } \| { dayOfWeek: number \| number[]; } \| (false \| true \| Date \| ((date: Date) => boolean) \| Date[] \| { from: Date; to?: Date; } \| { before: Date; } \| { after: Date; } \| { before: Date; after: Date; } \| { dayOfWeek: number \| number[]; }[])` | -                  | Matcher for disabled dates. This can be a single matcher or an array of matchers.                                                                                                                                                              |
| dismissible          | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                        | -                  | Whether to show a dismiss/clear button when the input has a value.                                                                                                                                                                             |
| endMonth             | `Date`                                                                                                                                                                                                                                                                                                                                                                                                                           | `5 years from now` | The latest month that can be selected from in the datepicker calendar.                                                                                                                                                                         |
| excludeDisabledDates | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                        | -                  | When true, excludes disabled dates from range.                                                                                                                                                                                                 |
| format               | `string`                                                                                                                                                                                                                                                                                                                                                                                                                         | `"M/d/yyyy"`       | The date format string using `date-fns` format syntax. Used to format the date displayed in the input field and as the default placeholder text. When `formatFn` is provided, this prop is ignored in favor of the custom formatting function. |
| formatFn             | `(date: Date) => string`                                                                                                                                                                                                                                                                                                                                                                                                         | -                  | Custom formatting function for displaying dates in the input field. When provided, this takes precedence over the `format` prop. It's recommended to set a relevant `placeholder` when supplying a custom format function.                     |
| kind                 | `"single" \| "range"`                                                                                                                                                                                                                                                                                                                                                                                                            | -                  | The kind of datepicker.                                                                                                                                                                                                                        |
| max                  | `number`                                                                                                                                                                                                                                                                                                                                                                                                                         | -                  | Maximum number of nights in a range.                                                                                                                                                                                                           |
| min                  | `number`                                                                                                                                                                                                                                                                                                                                                                                                                         | -                  | Minimum number of nights in a range.                                                                                                                                                                                                           |
| modal                | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                        | `false`            | The modality of the datepicker. When set to true, interaction with outside elements will be disabled and only datepicker content will be visible to screen readers. Modality is disabled by default to minimize performance impact.            |
| month                | `Date`                                                                                                                                                                                                                                                                                                                                                                                                                           | -                  | The controlled value of the month in the datepicker calendar. Must be used in conjunction with `onMonthChange`. Use this when you need to programmatically control which month is displayed.                                                   |
| onCloseAutoFocus     | `(event: Event) => void`                                                                                                                                                                                                                                                                                                                                                                                                         | -                  | Event handler called when focus moves to the trigger after closing. It can be prevented by calling `event.preventDefault`.                                                                                                                     |
| onEscapeKeyDown      | `(event: KeyboardEvent) => void`                                                                                                                                                                                                                                                                                                                                                                                                 | -                  | Event handler called when the escape key is down. It can be prevented by calling `event.preventDefault`.                                                                                                                                       |
| onMonthChange        | `(month: Date) => void`                                                                                                                                                                                                                                                                                                                                                                                                          | -                  | Event handler called when the month changes through navigation or dropdown selection. Use this to track or respond to month navigation within the calendar.                                                                                    |
| onOpenChange         | `(open: boolean) => void`                                                                                                                                                                                                                                                                                                                                                                                                        | -                  | Event handler called when the open state of the datepicker changes.                                                                                                                                                                            |
| onValueChange        | `(value: Date) => void \| (value: { from: Date; to?: Date; }) => void`                                                                                                                                                                                                                                                                                                                                                           | -                  | Event handler called when the value changes.                                                                                                                                                                                                   |
| open                 | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                        | -                  | The controlled open state of the datepicker. Must be used in conjunction with `onOpenChange`.                                                                                                                                                  |
| placeholder          | `string \| string \| { from?: string; to?: string; }`                                                                                                                                                                                                                                                                                                                                                                            | -                  | The placeholder text.                                                                                                                                                                                                                          |
| readOnly             | `boolean`                                                                                                                                                                                                                                                                                                                                                                                                                        | -                  | Whether the datepicker is read-only.                                                                                                                                                                                                           |
| renderDropdown       | `(props: { className?: string; children?: React.ReactNode; kind: "month" \| "year"; items: DropdownEntry[]; onItemSelect?: (event: Event, item: DropdownDefaultItemEntry \| DropdownCheckboxItemEntry \| DropdownRadioItemEntry) => void; }) => React.JSX.Element`                                                                                                                                                               | -                  | Custom function to render the dropdown for month and year selection. Use this to customize the appearance or behavior of the month/year dropdown menus. The props passed to this function use the standard DropdownProps interface.            |
| side                 | `"right" \| "bottom" \| "left" \| "top"`                                                                                                                                                                                                                                                                                                                                                                                         | `"bottom"`         | The preferred side of the trigger to render against when open. Will be reversed when collisions occur.                                                                                                                                         |
| size                 | `"small" \| "medium" \| "large"`                                                                                                                                                                                                                                                                                                                                                                                                 | `"medium"`         | The size of the input to render. Available sizes are "small", "medium", and "large".                                                                                                                                                           |
| startMonth           | `Date`                                                                                                                                                                                                                                                                                                                                                                                                                           | -                  | The earliest month that can be selected from in the datepicker calendar.                                                                                                                                                                       |
| status               | `"error" \| "success"`                                                                                                                                                                                                                                                                                                                                                                                                           | -                  | The status of the datepicker.                                                                                                                                                                                                                  |
| timeZone             | `string`                                                                                                                                                                                                                                                                                                                                                                                                                         | -                  | The time zone (IANA or UTC offset) to use.                                                                                                                                                                                                     |
| value                | `Date \| { from: Date; to?: Date; }`                                                                                                                                                                                                                                                                                                                                                                                             | -                  | The controlled value.                                                                                                                                                                                                                          |
