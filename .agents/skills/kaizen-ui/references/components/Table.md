<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Table

A table organizes sets of data into columns and rows.

## Notes

- On list-view pages, render Table directly inside the main content area without wrapping it in a Panel. Panel adds card-in-page chrome that compresses row width. Panel-wrapping is only appropriate for dashboard table widgets.

## Examples

### Basic Table

Use to display structured data in columns and rows for scanning and comparison.

```tsx
<Table
    columns={['Name', 'Role', 'Status']}
    rows={[
        { id: '1', cells: ['Alice', 'Engineer', 'Active'] },
        { id: '2', cells: ['Bob', 'Designer', 'Active'] }
    ]}
/>
```

### Hoverable Rows Table

Use when rows are interactive or clickable and a hover state helps indicate interactivity.

```tsx
<Table
    hoverableRows
    columns={['Name', 'Role', 'Status']}
    rows={[
        { id: '1', cells: ['Alice', 'Engineer', 'Active'] },
        { id: '2', cells: ['Bob', 'Designer', 'Active'] },
        { id: '3', cells: ['Charlie', 'Manager', 'Inactive'] }
    ]}
/>
```

### Selectable Rows Table

Use when users need to select rows for bulk actions or detail views.

```tsx
<Table
    hoverableRows
    columns={['Name', 'Email', 'Department']}
    rows={[
        {
            id: '1',
            cells: ['Alice', 'alice@example.com', 'Engineering'],
            onRowSelect: handleRowSelect,
            selected: true
        },
        {
            id: '2',
            cells: ['Bob', 'bob@example.com', 'Design'],
            onRowSelect: handleRowSelect
        },
        {
            id: '3',
            cells: ['Charlie', 'charlie@example.com', 'Product'],
            onRowSelect: handleRowSelect
        }
    ]}
/>
```

### Auto Layout Table

Use when column widths should adapt to their content rather than being evenly distributed.

```tsx
<Table
    layout="auto"
    columns={['ID', 'Description', 'Updated']}
    rows={[
        { id: '1', cells: ['NV-001', 'GPU cluster provisioning', '2 hours ago'] },
        {
            id: '2',
            cells: ['NV-002', 'Model training pipeline', '5 minutes ago']
        }
    ]}
/>
```

### Compact Density Table

Use when displaying dense data where vertical space is at a premium.

```tsx
<Table
    density="compact"
    columns={['Metric', 'Value', 'Change']}
    rows={[
        { id: '1', cells: ['Throughput', '1,200 req/s', '+12%'] },
        { id: '2', cells: ['Latency', '45ms', '-3%'] },
        { id: '3', cells: ['Error Rate', '0.02%', '-15%'] }
    ]}
/>
```

### Right Aligned Table

Use right alignment for columns of numerical data so digits line up across rows for easier comparison.

```tsx
<Table
    align="right"
    columns={['Region', 'Requests', 'Latency']}
    rows={[
        { id: '1', cells: ['us-east-1', '1,204,310', '45ms'] },
        { id: '2', cells: ['us-west-2', '982,117', '52ms'] },
        { id: '3', cells: ['eu-central-1', '604,889', '61ms'] }
    ]}
/>
```

### Selectable Columns Table

Use a column or cell object with onColumnSelect / onCellSelect when individual headers or cells need to be keyboard-focusable and interactive, for example to drive sorting or per-cell actions.

```tsx
<Table
    columns={[
        { children: 'Name', onColumnSelect: handleColumnSelect },
        { children: 'Email', onColumnSelect: handleColumnSelect },
        'Role'
    ]}
    rows={[
        {
            id: '1',
            cells: [
                'Jensen Huang',
                'jensen@nvidia.com',
                { children: 'Admin', onCellSelect: handleCellSelect }
            ]
        },
        {
            id: '2',
            cells: [
                'Cole Palmer',
                'cole@nvidia.com',
                { children: 'User', onCellSelect: handleCellSelect }
            ]
        }
    ]}
/>
```

### Rich Cells Table

Use cell objects with a JSX children when a column needs to render rich content like avatars, badges, or inline action buttons rather than plain text.

```tsx
<Table
    columns={['User', 'Status', 'Actions']}
    rows={[
        {
            id: '1',
            cells: [
                {
                    children: (
                        <Flex gap="density-md" align="center">
                            <Avatar fallback="MW" />
                            <span>Matt Woppler</span>
                        </Flex>
                    )
                },
                {
                    children: (
                        <Badge color="green" kind="solid">
                            Active
                        </Badge>
                    )
                },
                {
                    children: (
                        <Button kind="secondary" size="small">
                            Edit
                        </Button>
                    )
                }
            ]
        }
    ]}
/>
```

### With Toolbar Table

Pair a Table with a TableToolbar to host primary actions and a bulk-action bar. Toggle showBulkActionsToolbar based on whether any rows are selected to swap between the two states.

```tsx
<div>
    <TableToolbar
        showBulkActionsToolbar
        slotBulkActions={
            <>
                <Text>1 user selected</Text>
                <Button color="danger" kind="tertiary">
                    Delete
                </Button>
            </>
        }
    >
        <Text>1 user</Text>
        <Button color="brand">Add New</Button>
    </TableToolbar>
    <Table
        columns={['Name', 'Email']}
        rows={[{ id: '1', cells: ['Jensen Huang', 'jensen@nvidia.com'] }]}
    />
</div>
```

### Composed

```tsx
<TableRoot hoverableRows>
    <TableHead>
        <TableRow>
            <TableHeaderCell>Name</TableHeaderCell>
            <TableHeaderCell>Role</TableHeaderCell>
            <TableHeaderCell>Status</TableHeaderCell>
        </TableRow>
    </TableHead>
    <TableBody>
        <TableRow>
            <TableDataCell>Alice</TableDataCell>
            <TableDataCell>Engineer</TableDataCell>
            <TableDataCell>Active</TableDataCell>
        </TableRow>
        <TableRow>
            <TableDataCell>Bob</TableDataCell>
            <TableDataCell>Designer</TableDataCell>
            <TableDataCell>Active</TableDataCell>
        </TableRow>
    </TableBody>
</TableRoot>
```

## Props

| Prop           | Type                                                                                                                                                                                                    | Default      | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **columns** \* | `string \| ({ children: ReactNode; onColumnSelect: (_: { columnIndex: number; }) => void })[]`                                                                                                          | -            | Definitions for each column in the table                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| **rows** \*    | `{ id: string; cells: string \| ({ children: ReactNode; onCellSelect: (_: { rowId?: string; columnIndex: number; }) => void })[]; onRowSelect: (_: { rowId?: string; }) => void; selected: boolean }[]` | -            | Data for each row in the table                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| align          | `"center" \| "right" \| "left"`                                                                                                                                                                         | `"left"`     | Where to align table content. This will set alignment for all cells in the table. In general, textual data should be left-aligned and numerical data should be right-aligned. Consider how mixing different alignments across columns can affect readability.                                                                                                                                                                                                                                                                                                                            |
| density        | `"compact" \| "standard" \| "spacious"`                                                                                                                                                                 | `"standard"` | The "density" of the component. This affects the padding/spacing of the component and its children. By default, the component will inherit density from its parent, which is usually `standard`. - Setting to `null` or `undefined` will allow the component to inherit density from its parent. - Setting to `compact` will reduce the padding/spacing of the component and its children. - Setting to `standard` will use the standard padding/spacing of the component and its children. - Setting to `spacious` will increase the padding/spacing of the component and its children. |
| hoverableRows  | `boolean`                                                                                                                                                                                               | `false`      | Whether the table should have hover states for rows.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| layout         | `"auto" \| "fixed"`                                                                                                                                                                                     | `"fixed"`    | The layout variant for the table                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| ref            | `React.RefObject<HTMLTableElement> \| ((instance: HTMLTableElement) => void \| (() => void \| { [UNDEFINED_VOID_ONLY]: never; }))`                                                                      | -            | Allows getting a ref to the component instance. Once the component unmounts, React will set `ref.current` to `null` (or call the ref with `null` if you passed a callback ref).                                                                                                                                                                                                                                                                                                                                                                                                          |

`* = required prop`
