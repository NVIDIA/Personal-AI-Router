<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Card

A card contains content and actions about a single subject. Cards carry identity: they describe
"a thing" with its own content, actions, and optional media. By default cards are fluid and will
grow to fit their container.

## Notes

- The Card already has padding, so do not add padding to the Card component or to it's children.

- Card is for single-subject entity display (one cluster, one user, one model). Do not use Card as a general container on dashboards — use Panel instead. Card's interaction states and internal structure add unintended visual layering when used as a generic wrapper.

## Examples

### Basic Card That Fits Its Content

```tsx
<Card className="h-fit">
    <Badge kind="solid" color="gray">
        Badge
    </Badge>
    <Text kind="body/bold/2xl">Header</Text>
    <Text kind="body/regular/md">Lorem ipsum dolor sit amet</Text>
</Card>
```

### With Header Card

The header slot is rendered absolutely over the media. Use it to add a persistent label or badge above the body content. If there is no media, the header will be rendered above the body content. It is a flex container with a preset gap.

```tsx
<Card
    slotHeader={
        <Flex gap="inherit">
            <Badge>New</Badge>
            <Badge>Alt</Badge>
        </Flex>
    }
>
    Card body content
</Card>
```

### With Media Card

Use when the card needs a visual hero area like an image or video above or alongside the content. If the media rendered is dark or light, and you're using `slotHeader` in conjunction with it, you may want to set `mediaTheme` to ensure text and components are readable. This will set light/dark theme in the header slot to ensure sufficient contrast.

```tsx
<Card slotHeader={<span>Featured</span>} slotMedia={<MediaImg />} mediaTheme="light">
    Card content below media
</Card>
```

### Realistic Example Card

```tsx
<Card
    interactive
    layout="horizontal"
    kind="solid"
    slotHeader={
        <Flex gap="inherit" wrap="wrap">
            <Badge kind="solid" color="purple">
                Preview
            </Badge>
            <Badge kind="solid" color="teal">
                Secure
            </Badge>
        </Flex>
    }
    slotMedia={<img className="brightness-75" src="model.jpg" alt="AI model visualization" />}
    mediaTheme="dark"
>
    <Text kind="label/semibold/lg">NVIDIA</Text>
    <Text kind="body/bold/2xl">Llama 3.3 Nemotron Super 49B v1.5</Text>
    <Text kind="body/regular/md">
        Llama 3.3 Nemotron Super 49B v1.5 is a high-performance inference platform for AI models.
    </Text>
    <Flex gap="2" wrap="wrap">
        <Tag>Tag Text</Tag>
        <Tag>Tag Text</Tag>
        <Tag>Tag Text</Tag>
    </Flex>
</Card>
```

### Interactive Card

Use when the entire card should be a clickable target, such as navigation or selection. Render as a link for navigation or a button for actions. If the entire card is interactive, avoid adding nesting interactive elements in the content.

```tsx
<Card asChild interactive>
    <button type="button" onClick={handleClick}>
        <Flex direction="col" gap="1">
            <Text className="text-secondary" kind="label/bold/sm">
                DeepSeek
            </Text>
            <Text kind="label/bold/md">DeepSeek-V3.1</Text>
            <Text kind="body/regular/sm">
                DeepSeek V3.1 Instruct is a hybrid AI model with fast reasoning, 128K context, and
                strong tool use.
            </Text>
        </Flex>
    </button>
</Card>
```

### With Actions

```tsx
<Card>
    Card content... Right aligned actions:
    <Flex gap="2" justify="end" wrap="wrap">
        <Button kind="tertiary">Export</Button>
        <Button kind="secondary">Share</Button>
    </Flex>
    Or you can have actions stretch across the card:
    <Flex gap="2" justify="end" wrap="wrap">
        <Button kind="tertiary" style={{ flex: 1 }}>
            Export
        </Button>
        <Button kind="secondary" style={{ flex: 1 }}>
            Share
        </Button>
    </Flex>
    Or for just a single action:
    <Button color="brand" style={{ width: '100%' }}>
        Share
    </Button>
</Card>
```

### Cards In Responsive Grid

It's common to use cards in a responsive grid. Using the grid component, set a minimum column width and let the cards automatically resize to fill the available space.

```tsx
<Grid colMinWidth="250px" gap="2">
    <Card>Card 1</Card>
    <Card>Card 2</Card>
    <Card>Card 3</Card>
</Grid>
```

### Composed

Use composed primitives when you need full control over the card structure and slot placement.

```tsx
<Flex direction="col" gap="1">
    <CardRoot interactive={false} kind="solid">
        <CardMedia mediaTheme="light" slotHeader={<Badge>Featured</Badge>}>
            <MediaImg />
        </CardMedia>
        <CardContent>Composed card with media</CardContent>
    </CardRoot>
    <CardRoot>
        <CardContent>
            <div className="nv-card-content-header">
                <Badge>Featured</Badge>
            </div>
            Composed card without media
        </CardContent>
    </CardRoot>
</Flex>
```

## Props

| Prop        | Type                                    | Default      | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| ----------- | --------------------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| asChild     | `boolean`                               | -            | When true, renders the immediate child instead of the default element, merging this component's props with the child's props.                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| density     | `"compact" \| "standard" \| "spacious"` | `"standard"` | The "density" of the component. This affects the padding/spacing of the component and its children. By default, the component will inherit density from its parent, which is usually `standard`. - Setting to `null` or `undefined` will allow the component to inherit density from its parent. - Setting to `compact` will reduce the padding/spacing of the component and its children. - Setting to `standard` will use the standard padding/spacing of the component and its children. - Setting to `spacious` will increase the padding/spacing of the component and its children. |
| interactive | `boolean`                               | `false`      | If true, the card will be interactive and will show a hover effect. Do not enable on non-clickable cards - it creates false affordance.                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| kind        | `"solid" \| "gradient" \| "float"`      | `"solid"`    | Card variants. `solid` - renders the media and content in a single card with padding around the content and a hard border between the media and content. Use for most cards. `gradient` - the same as `solid` but with the media fading out to the content. Use for promotional or "hero-like" cards `float` - Should be used with `slotMedia`. This renders the media as a card with its own border, and renders the content without any padding or background. Use for cards that need to visually lift off the background.                                                            |
| layout      | `"horizontal" \| "vertical"`            | `"vertical"` | Sets the orientation of the card. `horizontal` will place the media to the left of the content. Use `vertical` when the image is the focal point and the content is lightweight. Cards typically display in a grid, saving horizontal space. Use `horizontal` when the content is more important than the image and typically stack vertically.                                                                                                                                                                                                                                          |
| mediaTheme  | `"light" \| "dark"`                     | -            | `slotHeader` is rendered overtop of media. Depending on the colour of your media, you may want your `slotHeader` content to render in light or dark theme to provide the best contrast. Set `mediaTheme` to override the theme on the card media. For example - if you're using a dark image you should set `mediaTheme="light"`.                                                                                                                                                                                                                                                        |
| selected    | `boolean`                               | `false`      | If true, the card will be "selected" and will show a selected state.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| slotHeader  | `ReactNode`                             | -            | The header content of the Card. If `slotMedia` is provided, this will be rendered absolutely over the media. Default styles: `flex gap-2 flex-wrap`                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| slotMedia   | `ReactNode`                             | -            | The media content of the card. If you are using `slotHeader` in conjunction with this, you should probably set `mediaTheme` to ensure text and components are readable. For light media, you should set `mediaTheme="dark"` to render header content in dark theme. For dark media, you should set `mediaTheme="light"`.                                                                                                                                                                                                                                                                 |
