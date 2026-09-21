<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Pagination

A pagination component that provides different styles of navigation for paginated content.

## Notes

- Only render Pagination when totalItems > pageSize. When the dataset fits on a single page, Pagination is unnecessary — conditionally render it.

## Examples

### Basic Pagination

Use for large datasets where users may need to jump to a specific page by number.

```tsx
<Pagination totalItems={100} defaultPage={1} defaultPageSize={10} />
```

### Tabs Pagination

Use for moderate-sized datasets where users benefit from seeing clickable page numbers.

```tsx
<Pagination kind="tabs" totalItems={100} defaultPage={1} defaultPageSize={10} />
```

### Simple Pagination

Use for linear navigation where only previous and next actions are needed.

```tsx
<Pagination kind="simple" totalItems={100} defaultPage={1} defaultPageSize={10} />
```

### With Controls Pagination

Use when users need to adjust page size and see the current item range.

```tsx
<Pagination displayControls totalItems={100} defaultPage={1} defaultPageSize={10} />
```

### Controlled Pagination

Pass `page` + `onPageChange` (and `pageSize` + `onPageSizeChange`) when pagination state lives outside the component — e.g. synced with URL params, a query cache, or a server response.

```tsx
<Pagination
    totalItems={100}
    page={1}
    pageSize={10}
    onPageChange={handlePageChange}
    onPageSizeChange={handlePageSizeChange}
/>
```

### With Custom Range Text Pagination

Pass `rangeTextFormatFn` to override the default `"1-10 of 100 items"` label. Receives `{ firstItemIndex, lastItemIndex, totalItems, pageSize }` and returns a `ReactNode`. Requires `displayControls`.

```tsx
<Pagination
    displayControls
    totalItems={100}
    defaultPage={1}
    defaultPageSize={10}
    rangeTextFormatFn={range =>
        `Showing ${range.firstItemIndex}–${range.lastItemIndex} of ${range.totalItems} results`
    }
/>
```

### Tabs As Links Pagination

Pass custom `items` with `href` to render each page tab as an anchor — ideal for SSR navigation where the URL is the source of truth instead of React state.

```tsx
<Pagination
    kind="tabs"
    totalItems={100}
    defaultPage={1}
    defaultPageSize={10}
    items={Array.from({ length: 10 }, (_, i) => ({
        value: i + 1,
        children: i + 1,
        href: `?page=${i + 1}`
    }))}
/>
```

### Form Submission Pagination

Set `action="form"` and wrap in a `<form>` to render the arrow buttons, page input, and page size select as native submit controls. Parse the submission with `parsePaginationFormData(formData, { submitter, totalItems })` to support progressive enhancement (works without JS) and server-driven pagination.

```tsx
<form
    onSubmit={event => {
        event.preventDefault()
        const submitter = (event.nativeEvent as SubmitEvent).submitter
        const formData = new FormData(event.currentTarget)
        parsePaginationFormData(formData, { submitter, totalItems: 100 })
    }}
>
    <Pagination
        action="form"
        displayControls
        totalItems={100}
        defaultPage={1}
        defaultPageSize={10}
    />
</form>
```

### Cursor Based Pagination

Override `name` and `value` on individual arrow buttons via `attributes` when integrating with cursor-based APIs that submit opaque cursors instead of page numbers.

```tsx
<form>
    <Pagination
        kind="simple"
        action="form"
        totalItems={100}
        page={2}
        pageSize={10}
        attributes={{
            PaginationArrowButtonPrevious: {
                name: 'cursor',
                value: 'prev_abc123'
            },
            PaginationArrowButtonNext: {
                name: 'cursor',
                value: 'next_xyz789'
            }
        }}
    />
</form>
```

### Tabs Form Submission Pagination

When pairing `kind="tabs"` with `action="form"`, supply custom `items` rendered as `<Button type="submit" name="page">` so each tab submits its page number — the default tab triggers cannot auto-convert to form buttons.

```tsx
<form>
    <Pagination
        kind="tabs"
        action="form"
        totalItems={50}
        defaultPage={1}
        defaultPageSize={10}
        items={Array.from({ length: 5 }, (_, i) => ({
            value: i + 1,
            asChild: true,
            children: (
                <Button type="submit" name="page" value={i + 1} kind="tertiary">
                    {i + 1}
                </Button>
            )
        }))}
    />
</form>
```

### Composed

```tsx
<PaginationRoot totalItems={100} defaultPage={1} defaultPageSize={10}>
    <PaginationNavigationGroup>
        <PaginationArrowButton direction="first" />
        <PaginationArrowButton direction="previous" />
        <PaginationPageInput />
        <PaginationPageCountText
        // pageCountTextFormatFn={(pageMeta) => `of ${pageMeta.total}`}
        />
        <PaginationArrowButton direction="next" />
        <PaginationArrowButton direction="last" />
    </PaginationNavigationGroup>
</PaginationRoot>
```

## Props

| Prop                   | Type                                                                                                                         | Default             | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------- | ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **displayControls** \* | `true \| false`                                                                                                              | `false`             | Whether to display the controls (page size select and associated item range text)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| **totalItems** \*      | `number`                                                                                                                     | -                   | The total number of items. Used to calculate the number of pages and the item range text.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| action                 | `"form"`                                                                                                                     | -                   | The action mode for the pagination component. - `undefined`: Default behavior using React state management (controlled/uncontrolled). - `"form"`: Enables native form submission. Child components will render as submit buttons with appropriate `name` and `value` attributes, skipping internal React state updates. Useful for server-driven pagination where the URL is the source of truth.                                                                                                                                                                                     |
| asChild                | `boolean`                                                                                                                    | -                   | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| defaultPage            | `number`                                                                                                                     | `1`                 | The initial value of the page. This is used when first rendering the component but does not maintain control over state changes after initial rendering.                                                                                                                                                                                                                                                                                                                                                                                                                              |
| defaultPageSize        | `number`                                                                                                                     | `10`                | The initial value of the page size. This is used when first rendering the component but does not maintain control over state changes after initial rendering.                                                                                                                                                                                                                                                                                                                                                                                                                         |
| items                  | `never \| Omit<TabItem, "value"> & { value: number; }[]`                                                                     | -                   |                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| kind                   | `"input" \| "tabs" \| "simple"`                                                                                              | `"input"`           | The kind of pagination to render. Determines what components are displayed. - `simple`: A basic previous and next button - meant for linear navigation, providing users the ability to move forwards or backwards in a sequence. - `input`: An input for navigating to a specific page between first, next, previous, and last buttons - meant for large datasets where users may want to jump to a specific page - `tabs`: A list of page numbers as tabs between first, next, previous, and last buttons - meant for moderate-sized datasets allowing users to skim through a range |
| onPageChange           | `(page: number) => void`                                                                                                     | -                   | Callback fired when the page changes.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| onPageSizeChange       | `(pageSize: number) => void`                                                                                                 | -                   | Callback fired when the page size changes.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| page                   | `number`                                                                                                                     | -                   | The value of the page. This is used to control which item is open in a controlled component setup. If not provided, the component will be uncontrolled and manage its own state internally.                                                                                                                                                                                                                                                                                                                                                                                           |
| pageMeta               | `{ first: number; last: number; total: number }`                                                                             | -                   | Override metadata for the page range. Useful for overriding the default page range bounds.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| pageSize               | `number`                                                                                                                     | -                   | The value of the page size. This is used to control which item is open in a controlled component setup. If not provided, the component will be uncontrolled and manage its own state internally.                                                                                                                                                                                                                                                                                                                                                                                      |
| pageSizeOptions        | `number[]`                                                                                                                   | `[10, 25, 50, 100]` | The available page sizes for the page size select                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| rangeMeta              | `{ firstItemIndex: number; lastItemIndex: number; totalItems: number; pageSize: number }`                                    | -                   | Override meta data for the item range. Useful for overriding the default item range bounds.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| rangeTextFormatFn      | `(rangeMeta: { firstItemIndex: number; lastItemIndex: number; totalItems: number; pageSize: number }) => ReactNode \| never` | -                   | Optional custom format function for the range text that is rendered when `displayControls` is true.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| renderedItemCount      | `never \| 7 \| 5 \| 6 \| 8 \| 9 \| 10`                                                                                       | -                   |                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |

`* = required prop`
