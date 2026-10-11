<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# CodeSnippet

A code snippet component with syntax highlighting, copy functionality, and optional collapse behaviour.
Supports both inline code spans and block-level code blocks.

## Examples

### Basic Code Snippet

Use for displaying code blocks with syntax highlighting and a copy button.

```tsx
<CodeSnippet language="javascript" value="console.log('Hello, world!');" />
```

### Inline Code Snippet

Use when referencing code inline within a paragraph of text.

```tsx
<p>
    Use the <CodeSnippet kind="inline" value="useState" language="typescript" /> hook to manage
    state.
</p>
```

### Collapsible Code Snippet

Use when displaying long code that should be collapsed by default to save vertical space.

```tsx
<CodeSnippet value={longCode} language="typescript" collapsible rows={5} />
```

### With Copy Callback Code Snippet

Use when you need to track or respond to copy events, such as for analytics.

```tsx
<CodeSnippet value={sampleCode} language="typescript" onCopySuccess={handleCopy} />
```

### With Custom Actions Code Snippet

Use when you need to add custom actions to the code snippet. For Buttons, prefer 'tiny' size.

```tsx
<CodeSnippet
    value={sampleCode}
    language="typescript"
    slotActions={
        <Button className="mr-auto" kind="secondary" size="tiny">
            Run
        </Button>
    }
/>
```

### With Fixed Theme

By default, the code snippet will render in the current theme. You can fix the theme to light or dark by applying the light or dark class to the CodeSnippet or its parent

```tsx
<div className="nv-dark bg-surface-base">
    <CodeSnippet value={sampleCode} language="typescript" />
</div>
```

### With Server Side Rendering Code Snippet

Use when you need to server-side render the code snippet.

```tsx
;async () => {
    // use highlightCode util to server side highlight the code
    const prerenderedCode = await highlightCode(sampleCode, 'typescript')
    return <CodeSnippet defaultValue={prerenderedCode} language="typescript" value={sampleCode} />
}
```

### Composed

```tsx
<Flex>
    Block:
    <CodeSnippetRoot kind="block" collapsible rows={4}>
        <CodeSnippetActions>
            <Button kind="secondary" size="tiny">
                Custom Action
            </Button>
            <CodeSnippetCopyButton value={sampleCode} />
            <CodeSnippetExpanderButton />
        </CodeSnippetActions>
        <CodeSnippetCode value={sampleCode} language="typescript" />
    </CodeSnippetRoot>
    Inline:
    <CodeSnippetRoot kind="inline">
        <CodeSnippetCode value={sampleCode} language="typescript" />
    </CodeSnippetRoot>
</Flex>
```

## Props

| Prop          | Type                                                                                                                                                               | Default                    | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **kind** \*   | `"inline" \| "block"`                                                                                                                                              | `"block"`                  | The kind of code snippet to render.                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| **value** \*  | `string`                                                                                                                                                           | -                          | The code to display in the code snippet.                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| collapsible   | `never \| boolean`                                                                                                                                                 | -                          | Whether the code snippet is collapsible. Only valid when kind is "block".                                                                                                                                                                                                                                                                                                                                                                                                                              |
| defaultOpen   | `boolean`                                                                                                                                                          | `false`                    | Default open state for uncontrolled mode.                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| defaultValue  | `string`                                                                                                                                                           | -                          | Pre-rendered HTML for the code snippet. If provided, skips client-side highlighting entirely. **For SSR**: Pass server-rendered HTML here to ensure perfect hydration without re-highlighting.                                                                                                                                                                                                                                                                                                         |
| language      | `"html" \| "text" \| "go" \| "typescript" \| "javascript" \| "tsx" \| "jsx" \| "json" \| "css" \| "bash" \| "shell" \| "python" \| "rust" \| "yaml" \| "markdown"` | `"text"`                   | The language to use for syntax highlighting.                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| onCopySuccess | `() => void`                                                                                                                                                       | -                          | The callback to call when the code snippet is copied successfully                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| onOpenChange  | `(open: boolean) => void`                                                                                                                                          | -                          | Callback fired when the open state changes.                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| open          | `boolean`                                                                                                                                                          | -                          | Controlled open state.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| rows          | `never \| number`                                                                                                                                                  | -                          | The number of rows to show. If collapsible is true, this applies when collapsed. If collapsible is false, this sets a fixed height with scroll. Only valid when kind is "block".                                                                                                                                                                                                                                                                                                                       |
| slotActions   | `ReactNode`                                                                                                                                                        | -                          | The actions to display on top of the code snippet.                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| structure     | `"inline" \| "classic"`                                                                                                                                            | `Derived from `kind` prop` | The HTML structure to use for rendering. - `classic`: Uses block-level elements (`<div>`, `<pre>`, `<code>`) - cannot be nested inside `<p>` tags - `inline`: Uses inline elements (`<span>`) - safe to nest inside `<p>` tags (e.g., from Markdown) If not specified, defaults based on `kind`: - `kind="block"` → `structure="classic"` - `kind="inline"` → `structure="inline"` Override this when you need block-styled code (with actions, copy button, etc.) but must render inside a `<p>` tag. |

`* = required prop`
