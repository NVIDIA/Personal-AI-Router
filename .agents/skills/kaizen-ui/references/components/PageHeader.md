<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# PageHeader

The page header describes the current page and displays a title, breadcrumbs, and page-level actions

## Notes

- Do not place back-navigation buttons ("Back to [list]") in slotActions. Back navigation belongs in slotBreadcrumbs via the Breadcrumbs component.

- Use kind="flat" for list/browse views and dashboards. The "floating" variant is for detail pages.

## Examples

### Basic Page Header

Use at the top of a page to describe its purpose with a title and optional description.

```tsx
<PageHeader
    slotSubheading="Subheading"
    slotHeading="Page Title"
    slotDescription="A brief description of what this page contains."
/>
```

### Floating Page Header

Use on detail pages where the header should read as a card and stand apart from page content.

```tsx
<PageHeader
    kind="floating"
    slotSubheading="Subheading"
    slotHeading="Page Title"
    slotDescription="A brief description of what this page contains."
/>
```

### With Breadcrumbs Page Header

Use for back-navigation and location context on nested pages. Back navigation belongs here, never in slotActions.

```tsx
<PageHeader
    slotBreadcrumbs={
        <Breadcrumbs
            items={[
                { children: <a href="/">Home</a> },
                { children: <a href="/models">Models</a> },
                'Llama 3.1 405B'
            ]}
        />
    }
    slotSubheading="NVIDIA"
    slotHeading="Llama 3.1 405B"
    slotDescription="A large language model optimized for dialogue and instruction-following tasks."
/>
```

### With Actions Page Header

Use when the page has primary actions like create, deploy, or export that belong in the header.

```tsx
<PageHeader
    slotHeading="Clusters"
    slotDescription="Manage your compute clusters and infrastructure."
    slotActions={
        <>
            <Button kind="secondary">Export</Button>
            <Button color="brand">Create Cluster</Button>
        </>
    }
/>
```

### With Non Stretching Actions Page Header

Wrap actions in a Flex container when the buttons should keep their natural size instead of stretching to fill the footer on narrow viewports.

```tsx
<PageHeader
    slotHeading="Page Title"
    slotSubheading="Subheading"
    slotDescription="A brief description of what this page contains."
    slotActions={
        <Flex gap="density-md" align="end">
            <Button kind="secondary">Secondary</Button>
            <Button color="brand">Primary</Button>
        </Flex>
    }
/>
```

### With Custom Heading Page Header

Pass JSX into slotHeading when the title needs an inline adornment such as a status badge or a custom heading element.

```tsx
<PageHeader
    slotSubheading="Subheading"
    slotHeading={
        <Flex gap="density-md" align="center">
            <h1>Page Title</h1>
            <Badge>Coming soon</Badge>
        </Flex>
    }
    slotDescription="A brief description of what this page contains."
>
    <Badge kind="solid">Language</Badge>
</PageHeader>
```

### Composed

```tsx
<PageHeaderRoot>
    <PageHeaderContainer kind="floating">
        <PageHeaderContent>
            <PageHeaderHeader>
                <PageHeaderSubheading>Subheading</PageHeaderSubheading>
                <PageHeaderHeading>Page Title</PageHeaderHeading>
                <PageHeaderDescription>
                    A brief description of what this page contains.
                </PageHeaderDescription>
            </PageHeaderHeader>
            <PageHeaderMain>Additional content area</PageHeaderMain>
        </PageHeaderContent>
        <PageHeaderFooter>
            <Button color="brand">Action</Button>
        </PageHeaderFooter>
    </PageHeaderContainer>
</PageHeaderRoot>
```

## Props

| Prop            | Type                                                                                                                           | Default      | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------ | ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| children        | `ReactNode`                                                                                                                    | -            | The children of the PageHeader. Renders below `slotDescription` with a large gap. Use for additional content such as Tags, Badges, Progress Bars, etc.                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| density         | `"compact" \| "standard" \| "spacious"`                                                                                        | `"standard"` | The "density" of the component. This affects the padding/spacing of the component and its children. By default, the component will inherit density from its parent, which is usually `standard`. - Setting to `null` or `undefined` will allow the component to inherit density from its parent. - Setting to `compact` will reduce the padding/spacing of the component and its children. - Setting to `standard` will use the standard padding/spacing of the component and its children. - Setting to `spacious` will increase the padding/spacing of the component and its children. |
| kind            | `"flat" \| "floating"`                                                                                                         | `"flat"`     | PageHeader variants. - `floating`: A PageHeader rendered in a container with a background and padding, creating separation from surrounding content. - `flat`: A PageHeader rendered with no padding, border, or background color. Ideal for lightweight experiences where content blends seamlessly.                                                                                                                                                                                                                                                                                    |
| ref             | `((instance: HTMLDivElement) => void \| (() => void \| { [UNDEFINED_VOID_ONLY]: never; })) \| React.RefObject<HTMLDivElement>` | -            | Allows getting a ref to the component instance. Once the component unmounts, React will set `ref.current` to `null` (or call the ref with `null` if you passed a callback ref).                                                                                                                                                                                                                                                                                                                                                                                                          |
| slotActions     | `ReactNode`                                                                                                                    | -            | Slot for the PageHeader footer. Handles spacing its children for you, and by default aligns the children to the right. Also handles responsive behaviour by stretching footer items on smaller screens to fill the width of the container.                                                                                                                                                                                                                                                                                                                                               |
| slotBreadcrumbs | `ReactNode`                                                                                                                    | -            | Slot for the breadcrumbs of the PageHeader. Renders above `slotSubheading`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| slotDescription | `ReactNode`                                                                                                                    | -            | Slot for the description of the PageHeader. With `body-regular-md` text styles                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| slotHeading     | `ReactNode`                                                                                                                    | -            | Slot for the heading of the PageHeader. With `body-bold-2xl` text styles, rendered below `slotSubheading` and above `slotDescription`.                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| slotSubheading  | `ReactNode`                                                                                                                    | -            | Slot for the subheading of the PageHeader. With `label-light-xl` text styles, rendered above `slotHeading`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
