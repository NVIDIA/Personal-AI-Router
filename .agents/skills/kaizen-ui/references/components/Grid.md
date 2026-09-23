<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Grid

A primitive grid component.

## Examples

### Basic Grid

Use auto-flowing columns for responsive layouts where items should wrap based on available space.

```tsx
<Grid colMinWidth="250px" gap="4">
    <div>Item 1</div>
    <div>Item 2</div>
    <div>Item 3</div>
</Grid>
```

### Fixed Columns Grid

Use a fixed column count when the layout should not reflow regardless of viewport width.

```tsx
<Grid cols={3} gap="4">
    <div>Column A</div>
    <div>Column B</div>
    <div>Column C</div>
</Grid>
```

### Responsive Columns Grid

Use responsive breakpoint columns when you need explicit control over column counts at each viewport size. Autoflowing columns is usually preferred over this.

```tsx
<Grid cols={{ sm: 1, md: 2, lg: 3 }} gap="4">
    <div>Column A</div>
    <div>Column B</div>
    <div>Column C</div>
</Grid>
```

### Static Columns Grid

Pair `colMinWidth` with `colBehavior="static"` for uniform card grids where stretching the last row would look awkward.

```tsx
<Grid colMinWidth="200px" colBehavior="static" gap="4">
    <div>Card 1</div>
    <div>Card 2</div>
    <div>Card 3</div>
</Grid>
```

### Column Flow Grid

Combine `rows` with `flow="col"` for column-major layouts where items fill top-to-bottom before wrapping to the next column.

```tsx
<Grid rows={3} flow="col" gap="2">
    <div>1</div>
    <div>2</div>
    <div>3</div>
    <div>4</div>
    <div>5</div>
    <div>6</div>
</Grid>
```

### Dense Flow Grid

Use `flow="dense"` with mixed `GridItem` spans to backfill gaps left by larger items, producing a tightly packed masonry-style layout.

```tsx
<Grid cols={4} flow="dense" gap="2">
    <GridItem cols={2}>Wide A</GridItem>
    <GridItem>B</GridItem>
    <GridItem rows={2}>Tall C</GridItem>
    <GridItem cols={3}>Wide D</GridItem>
    <GridItem>E</GridItem>
    <GridItem>F</GridItem>
</Grid>
```

### Composed

Use GridItem when individual cells need to span multiple columns or be placed at specific grid positions.

```tsx
<Grid cols={3} gap="4">
    <GridItem cols={2}>Spanning two columns</GridItem>
    <GridItem>Single column</GridItem>
    <GridItem colStart={2} colEnd={4}>
        Columns 2–3
    </GridItem>
</Grid>
```

## Props

type SpacingScaleUnion

Prefer semantic density tokens that adapt to the active density theme (compact, standard, spacious):
"density-xxs" | "density-xs" | "density-sm" | "density-md" | "density-lg" | "density-xl" | "density-2xl" | "density-3xl" | "density-4xl" | "density-5xl"

| Token       | Compact | Standard | Spacious |
| ----------- | ------- | -------- | -------- |
| density-xxs | 1px     | 2px      | 4px      |
| density-xs  | 2px     | 4px      | 6px      |
| density-sm  | 4px     | 6px      | 8px      |
| density-md  | 6px     | 8px      | 12px     |
| density-lg  | 8px     | 12px     | 16px     |
| density-xl  | 12px    | 16px     | 24px     |
| density-2xl | 16px    | 24px     | 32px     |
| density-3xl | 24px    | 32px     | 48px     |
| density-4xl | 32px    | 48px     | 64px     |
| density-5xl | 48px    | 64px     | 80px     |

Raw scale values (4px base unit — use only when density tokens don't fit):
"0" | "0.25" | "0.5" | "0.75" | "1" | "1.5" | "2" | "2.5" | "3" | "3.5" | "4" | "5" | "6" | "7" | "8" | "9" | "10" | "11" | "12" | "14" | "16" | "18" | "20" | "24" | "28" | "32" | "36" | "40" | "44" | "48" | "52" | "56" | "60" | "64" | "72" | "80" | "96" | "250" | "inherit" | "px"

| Prop          | Type                                                                                                          | Default  | Description                                                                                                                                                                                                                                                                           |
| ------------- | ------------------------------------------------------------------------------------------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild       | `boolean`                                                                                                     | -        | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                         |
| colBehavior   | `"grow" \| "static"`                                                                                          | `"grow"` | Determines column behavior when using `colMinWidth`. - `"grow"` (default): Columns expand to fill available space when there aren't enough items to fill a row - `"static"`: Columns maintain their minimum width, leaving empty space if needed Has no effect without `colMinWidth`. |
| colMinWidth   | `string \| number`                                                                                            | -        | Minimum width for auto-flowing columns. When set, creates responsive columns that automatically wrap based on available space. Overrides the `cols` prop when provided. Accepts CSS units (e.g., "250px", "15rem") or numbers (converted to px).                                      |
| cols          | `number \| { base?: number; xs?: number; sm?: number; md?: number; lg?: number; xl?: number; xxl?: number; }` | -        | Number of grid columns when working with row flow directions. This prop is ignored when `colMinWidth` is set.                                                                                                                                                                         |
| flow          | `string`                                                                                                      | -        | Grid auto-flow. See https://developer.mozilla.org/en-US/docs/Web/CSS/grid-auto-flow for more details.                                                                                                                                                                                 |
| gap           | `SpacingScaleUnion`                                                                                           | -        | Sets spacing between flex and grid items.                                                                                                                                                                                                                                             |
| padding       | `SpacingScaleUnion`                                                                                           | -        | Sets padding.                                                                                                                                                                                                                                                                         |
| paddingBottom | `SpacingScaleUnion`                                                                                           | -        | Sets bottom padding.                                                                                                                                                                                                                                                                  |
| paddingLeft   | `SpacingScaleUnion`                                                                                           | -        | Sets left padding.                                                                                                                                                                                                                                                                    |
| paddingRight  | `SpacingScaleUnion`                                                                                           | -        | Sets right padding.                                                                                                                                                                                                                                                                   |
| paddingTop    | `SpacingScaleUnion`                                                                                           | -        | Sets top padding.                                                                                                                                                                                                                                                                     |
| paddingX      | `SpacingScaleUnion`                                                                                           | -        | Sets horizontal padding.                                                                                                                                                                                                                                                              |
| paddingY      | `SpacingScaleUnion`                                                                                           | -        | Sets vertical padding.                                                                                                                                                                                                                                                                |
| rows          | `number \| { base?: number; xs?: number; sm?: number; md?: number; lg?: number; xl?: number; xxl?: number; }` | -        | Number of grid rows when working with column flow directions.                                                                                                                                                                                                                         |
