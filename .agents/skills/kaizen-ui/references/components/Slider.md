<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Slider

Also known as: Range Input, Input Range

A slider component allows users to select a single value from a range of values.

Sliders reflect a range of values along a bar, from which users may select a single value.
They are ideal for adjusting settings such as volume, brightness,
or applying image filters.

## Examples

### Basic Slider

```tsx
<Slider defaultValue={50} aria-label="Volume" />
```

### With Step Marks Slider

Use step marks to show discrete positions and help users pick precise values.

```tsx
<Slider defaultValue={40} step={10} stepPosition="end" aria-label="Brightness" />
```

### Custom Range Slider

Use custom min/max/step when the default 0–100 range does not match the domain.

```tsx
<Slider defaultValue={250} min={100} max={500} step={50} stepPosition="end" aria-label="Budget" />
```

### Disabled Slider

Use when the slider value is locked and cannot be changed.

```tsx
<Slider defaultValue={30} disabled aria-label="Locked" />
```

### Vertical Slider

Use vertical orientation for controls like volume faders or equalizer bands.

```tsx
<Slider
    defaultValue={60}
    orientation="vertical"
    stepPosition="start"
    step={20}
    aria-label="Level"
/>
```

### With Step Interval Slider

Use stepInterval to space the visible step markers at a coarser interval than the slider's underlying step value, so users can drag with fine precision while still seeing key reference points.

```tsx
<Slider
    defaultValue={50}
    min={0}
    max={100}
    step={1}
    stepInterval={20}
    stepPosition="end"
    aria-label="Precision control"
/>
```

### With Custom Steps Slider

Use customSteps when the meaningful tick positions are non-uniform or only certain anchor values matter (e.g. quality presets, named breakpoints). It overrides both step and stepInterval for marker placement.

```tsx
<Slider
    defaultValue={50}
    min={0}
    max={100}
    step={5}
    customSteps={[0, 25, 50, 75, 100]}
    stepPosition="end"
    aria-label="Quality preset"
/>
```

### With Formatted Steps Slider

Use stepFormatFn to render step labels in a domain-specific format like currency, percentages, units, or descriptive words (e.g. Mute / Quiet / Loud). Works with both orientations.

```tsx
<Slider
    defaultValue={60}
    min={0}
    max={100}
    step={1}
    stepInterval={20}
    stepPosition="end"
    stepFormatFn={value => `${value}°C`}
    aria-label="Temperature"
/>
```

### With Form Name Slider

Set name to submit the slider value as a standard form field — the underlying native range input participates in form submission with no extra wiring.

```tsx
<Slider name="volume" defaultValue={50} step={10} stepPosition="end" aria-label="Volume" />
```

### Composed

Use the composed primitives when you need full control over the slider track, input, and step label layout.

```tsx
<SliderRoot orientation="horizontal" aria-label="Custom slider">
    <SliderInput defaultValue={50} min={0} max={100} step={25} />
    <SliderSteps position="end" />
</SliderRoot>
```

## Props

| Prop          | Type                         | Default        | Description                                                                                                                                                    |
| ------------- | ---------------------------- | -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild       | `boolean`                    | -              | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                  |
| customSteps   | `number[]`                   | -              | Custom array of step values to display. When provided, overrides both step and stepInterval.                                                                   |
| defaultValue  | `number`                     | -              | The value of the slider when initially rendered. Use when you do not need to control the state of the slider.                                                  |
| max           | `number`                     | `100`          | The maximum value for the range.                                                                                                                               |
| min           | `number`                     | `0`            | The minimum value for the range.                                                                                                                               |
| onValueChange | `(value: number) => void`    | -              | Event handler called when the value changes.                                                                                                                   |
| onValueCommit | `(value: number) => void`    | -              | Event handler called when the value changes at the end of an interaction. Useful when you only need to capture a final value e.g. to update a backend service. |
| orientation   | `"horizontal" \| "vertical"` | `"horizontal"` | The orientation of the slider.                                                                                                                                 |
| step          | `number`                     | `1`            | The stepping interval.                                                                                                                                         |
| stepFormatFn  | `(value: number) => string`  | -              | Callback function to format the step value before displaying it.                                                                                               |
| stepInterval  | `number`                     | -              | The interval at which to display step markers. When not provided, uses the slider's step value.                                                                |
| stepPosition  | `"start" \| "end"`           | -              |                                                                                                                                                                |
| value         | `number`                     | -              | The controlled value of the slider. Must be used in conjunction with `onValueChange`.                                                                          |
