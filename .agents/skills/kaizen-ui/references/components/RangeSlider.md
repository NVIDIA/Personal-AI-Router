<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# RangeSlider

Also known as: Range Input, Input Range

A range slider component allows users to select a range of values (two thumbs).

Range sliders reflect a range of values along a bar, from which users may select
a range between two values. They are ideal for filtering by price ranges,
selecting time windows, or any scenario requiring a min/max selection.

## Examples

### Basic Range Slider

```tsx
<RangeSlider defaultValue={[25, 75]} aria-label="Price range" />
```

### With Step Marks Range Slider

Use step marks to show discrete positions and help users pick precise range boundaries.

```tsx
<RangeSlider defaultValue={[20, 80]} step={10} stepPosition="end" aria-label="Budget range" />
```

### With Formatted Steps Range Slider

Use stepFormatFn to render step labels in a domain-specific format like currency, percentages, or units.

```tsx
<RangeSlider
    defaultValue={[200, 800]}
    min={0}
    max={1000}
    step={100}
    stepPosition="end"
    stepFormatFn={value => `$${value}`}
    aria-label="Price range"
/>
```

### With Custom Steps Range Slider

Use customSteps when the meaningful values are non-uniform, like pricing tiers or named breakpoints.

```tsx
<RangeSlider
    defaultValue={[100, 400]}
    min={0}
    max={500}
    step={50}
    stepPosition="end"
    customSteps={[0, 100, 250, 500]}
    aria-label="Pricing tier range"
/>
```

### Custom Range Range Slider

Use custom min/max/step when the default 0–100 range does not match the domain.

```tsx
<RangeSlider
    defaultValue={[200, 800]}
    min={0}
    max={1000}
    step={100}
    stepPosition="end"
    aria-label="Price filter"
/>
```

### With Min Distance Range Slider

Use minStepsBetweenThumbs to enforce a minimum gap between the two thumbs, preventing collapsed selections.

```tsx
<RangeSlider
    defaultValue={[20, 80]}
    step={5}
    stepPosition="end"
    minStepsBetweenThumbs={4}
    aria-label="Date range"
/>
```

### Disabled Range Slider

Use when the range selection is locked and cannot be changed.

```tsx
<RangeSlider defaultValue={[30, 70]} disabled aria-label="Locked range" />
```

### Vertical Range Slider

Use vertical orientation for controls like equalizer bands or vertical filtering.

```tsx
<RangeSlider
    defaultValue={[20, 60]}
    orientation="vertical"
    stepPosition="start"
    step={20}
    aria-label="Level range"
/>
```

### With Form Name Range Slider

Set name to submit the range as two repeated form fields, supporting both hydrated and no-JS form submissions via the native fallback inputs.

```tsx
<RangeSlider
    name="priceRange"
    defaultValue={[100, 300]}
    min={0}
    max={500}
    step={25}
    stepPosition="end"
    aria-label="Price range"
/>
```

### Composed

Use the composed primitives when you need full control over track, range, thumb, and step label layout.

```tsx
<RangeSliderRoot defaultValue={[25, 75]} min={0} max={100} step={25} aria-label="Custom range">
    <RangeSliderTrack>
        <RangeSliderRange />
    </RangeSliderTrack>
    <RangeSliderThumb aria-label="Minimum" />
    <RangeSliderThumb aria-label="Maximum" />
    <RangeSliderSteps position="end" />
</RangeSliderRoot>
```

## Props

| Prop                  | Type                                | Default        | Description                                                                                                                                                    |
| --------------------- | ----------------------------------- | -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild               | `boolean`                           | -              | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                  |
| customSteps           | `number[]`                          | -              | Custom array of step values to display. When provided, overrides both step and stepInterval.                                                                   |
| defaultValue          | `[number, number]`                  | -              | The value of the slider when initially rendered. Use when you do not need to control the state of the slider.                                                  |
| disabled              | `boolean`                           | `false`        | When `true`, prevents the user from interacting with the slider.                                                                                               |
| form                  | `string`                            | -              | The ID of the form that the slider belongs to. If omitted, the slider will be associated with a parent form if one exists.                                     |
| max                   | `number`                            | `100`          | The maximum value for the range.                                                                                                                               |
| min                   | `number`                            | `0`            | The minimum value for the range.                                                                                                                               |
| minStepsBetweenThumbs | `number`                            | -              | The minimum permitted steps between multiple thumbs.                                                                                                           |
| name                  | `string`                            | -              | The name of the slider. Submitted with its owning form as part of a name/value pair.                                                                           |
| onValueChange         | `(value: [number, number]) => void` | -              | Event handler called when the value changes.                                                                                                                   |
| onValueCommit         | `(value: [number, number]) => void` | -              | Event handler called when the value changes at the end of an interaction. Useful when you only need to capture a final value e.g. to update a backend service. |
| orientation           | `"horizontal" \| "vertical"`        | `"horizontal"` | The orientation of the slider.                                                                                                                                 |
| step                  | `number`                            | `1`            | The stepping interval.                                                                                                                                         |
| stepFormatFn          | `(value: number) => string`         | -              | Callback function to format the step value before displaying it.                                                                                               |
| stepInterval          | `number`                            | -              | The interval at which to display step markers. When not provided, uses the slider's step value.                                                                |
| stepPosition          | `"start" \| "end"`                  | `undefined`    | The position of the step markers.                                                                                                                              |
| value                 | `[number, number]`                  | -              | The controlled value of the slider. Must be used in conjunction with `onValueChange`.                                                                          |
