<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Upload

Upload is a wrapper around `input type="file"` that also renders previews for uploaded files and
provides basic file management.

Note that file validation and actual upload is not handled by this component beyond what the input
element provides.

## Examples

### Basic Upload

```tsx
<Upload />
```

### With Accept Upload

Use when only specific file types should be accepted, and display the constraint to users.

```tsx
<Upload accept="image/png,application/pdf">Accepts: image/png, application/pdf</Upload>
```

### Multiple Upload

Use when users need to upload more than one file at a time.

```tsx
<Upload multiple>Up to 50 MB per file</Upload>
```

### With Default Value Upload

Use to pre-populate the file list with already-uploaded or in-progress items, e.g. when editing a saved record or resuming an interrupted session.

```tsx
<Upload
    multiple
    defaultValue={[
        {
            id: '1',
            file: new File(['resume content'], 'resume.pdf'),
            status: 'success',
            uploadedBytes: 1024
        },
        {
            id: '2',
            file: new File(['partial'], 'cover-letter.pdf'),
            status: 'uploading',
            uploadedBytes: 512
        },
        {
            id: '3',
            file: new File(['broken'], 'transcript.pdf'),
            status: 'error',
            errorMessage: 'Upload failed — please retry.'
        }
    ]}
/>
```

### Controlled Upload

Controlled mode lets you manage the selected files externally — useful for resetting after submission, syncing with form libraries, or showing custom UI based on file selection.

```tsx
;() => {
    const [files, setFiles] = useState<FileUploadItem[]>([])
    return <Upload multiple value={files} onValueChange={setFiles} />
}
```

### With Custom Trigger Content Upload

Override `slotAnchor` and `slotHeaderText` to tailor the trigger copy, and use children to surface inline constraints alongside the call to action.

```tsx
<Upload
    multiple
    accept=".pdf,.doc,.docx"
    slotAnchor="Browse for documents"
    slotHeaderText=" or simply drag them in."
>
    <span>Supported formats: PDF, Word</span>
    <span>·</span>
    <span>Maximum file size: 25 MB</span>
</Upload>
```

### With Callbacks Upload

Provide `onFileRetry` to expose a retry button on errored items, and `onFileRemove` to cancel in-flight requests or clean up server-side state when a file is removed.

```tsx
<Upload
    multiple
    defaultValue={[
        {
            id: '1',
            file: new File(['report content'], 'report.pdf'),
            status: 'error',
            errorMessage: 'Network error — please retry.'
        }
    ]}
    onFileRetry={item => {
        console.log('Retry upload for', item.file.name)
    }}
    onFileRemove={item => {
        console.log('Cancel upload for', item.file.name)
    }}
/>
```

### Media List Upload

Use `listKind="media"` for image and video uploads where a compact thumbnail grid is preferred over the default card layout.

```tsx
<Upload
    multiple
    accept="image/*,video/*"
    listKind="media"
    slotAnchor="Add media"
    slotHeaderText=" or drag photos and videos here."
    defaultValue={[
        {
            id: '1',
            file: new File(['img'], 'vacation.jpg', { type: 'image/jpeg' }),
            status: 'success'
        },
        {
            id: '2',
            file: new File(['vid'], 'demo.mp4', { type: 'video/mp4' }),
            status: 'uploading',
            uploadedBytes: 1
        }
    ]}
/>
```

### In Form Field Upload

Use `renderInput` to wrap the trigger in a `FormField` when the upload needs a label, help text, or needs to participate in a larger form layout.

```tsx
<Upload
    required
    renderInput={trigger => (
        <FormField
            name="documents"
            slotLabel="Upload documents"
            slotHelp="Files will be deleted after 30 days."
        >
            {trigger}
        </FormField>
    )}
/>
```

### Disabled Upload

Use when file upload should be temporarily unavailable based on form or permission state.

```tsx
<Upload disabled />
```

### With Validation Upload

Use when native validation cues should appear automatically via :user-valid and :user-invalid pseudo classes.

```tsx
<Upload required withValidation />
```

### Composed

Use composed primitives when you need full control over the trigger layout and file list rendering.

```tsx
<UploadRoot>
    <UploadTrigger>
        <UploadInputElement />
        <UploadDescription>Up to 50 MB</UploadDescription>
    </UploadTrigger>
    <UploadContent />
</UploadRoot>
```

## Props

| Prop           | Type                                                                                                                                                                                                                                                                                                                          | Default  | Description                                                                                                                                                                                                        |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| accept         | `string`                                                                                                                                                                                                                                                                                                                      | -        | MIME types accepted by the file input.                                                                                                                                                                             |
| children       | `ReactNode`                                                                                                                                                                                                                                                                                                                   | -        | Optional content to render in the trigger. This container has `label/regular/xs` text styles applied. Render additional context to the user here around file size limits, accept types, etc.                       |
| defaultValue   | `{ id: string; file: File; errorMessage: string; status: "error" \| "success" \| "uploading"; uploadedBytes: number; hidePreview: boolean } \| { id: string; file: File; errorMessage: string; status: "error" \| "success" \| "uploading"; uploadedBytes: number; hidePreview: boolean }[]`                                  | -        | The value of the upload when initially rendered. Use for uncontrolled state                                                                                                                                        |
| disabled       | `boolean`                                                                                                                                                                                                                                                                                                                     | `false`  | Whether the upload is disabled. Prevents file selection and drag and drop. You can also use this for read-only use cases.                                                                                          |
| kind           | `"flat" \| "floating"`                                                                                                                                                                                                                                                                                                        | `"flat"` | The kind of input to render. - "flat" - renders with a border and background - "floating" - renders borderless with no background                                                                                  |
| listKind       | `"media" \| "card"`                                                                                                                                                                                                                                                                                                           | `"card"` | Controls the format of the uploaded files. - `media` - use for images and videos, or compact lists - `card` - use for detailed file information                                                                    |
| multiple       | `false \| true`                                                                                                                                                                                                                                                                                                               | -        | Whether the upload allows multiple files.                                                                                                                                                                          |
| onFileRemove   | `(file: { id: string; file: File; errorMessage: string; status: "error" \| "success" \| "uploading"; uploadedBytes: number; hidePreview: boolean }) => void`                                                                                                                                                                  | -        | A callback triggered when a file is removed. Called with the file item that was removed. The file is still removed from the internal state regardless of whether this callback is provided.                        |
| onFileRetry    | `(file: { id: string; file: File; errorMessage: string; status: "error" \| "success" \| "uploading"; uploadedBytes: number; hidePreview: boolean }) => void`                                                                                                                                                                  | -        | A callback to handle file retry. If provided, we will render a retry button over files with a status of `error`. This callback will be called when the retry button is clicked.                                    |
| onValueChange  | `(file: { id: string; file: File; errorMessage: string; status: "error" \| "success" \| "uploading"; uploadedBytes: number; hidePreview: boolean }) => void \| (files: { id: string; file: File; errorMessage: string; status: "error" \| "success" \| "uploading"; uploadedBytes: number; hidePreview: boolean }[]) => void` | -        | Callback triggered whenever the value of the upload changes.                                                                                                                                                       |
| renderInput    | `(slotInput: ReactNode) => ReactNode`                                                                                                                                                                                                                                                                                         | -        | A render function to control rendering of the input element. Receives the default trigger component as an argument. Useful for wrapping the input element in another component, such as a `Tooltip` or `FormField` |
| showPreview    | `boolean`                                                                                                                                                                                                                                                                                                                     | `true`   | If false, we will not render the preview for the file.                                                                                                                                                             |
| slotAnchor     | `ReactNode`                                                                                                                                                                                                                                                                                                                   | -        | We render an anchor to act as the keyboard anchor for the upload trigger. If you want to render a custom anchor, you can pass it in here.                                                                          |
| slotHeaderText | `ReactNode`                                                                                                                                                                                                                                                                                                                   | -        | This is text that is rendered inline with the anchor text. This should be a continuination of the anchor text.                                                                                                     |
| status         | `"error" \| "success"`                                                                                                                                                                                                                                                                                                        | -        | The status of the input. Use `withValidation` to automatically apply success/error states based on `:user-valid` and `:user-invalid` pseudo classes.                                                               |
| value          | `{ id: string; file: File; errorMessage: string; status: "error" \| "success" \| "uploading"; uploadedBytes: number; hidePreview: boolean } \| { id: string; file: File; errorMessage: string; status: "error" \| "success" \| "uploading"; uploadedBytes: number; hidePreview: boolean }[]`                                  | -        | The value of the upload. Must be used in conjunction with `onValueChange`.                                                                                                                                         |
| withValidation | `boolean`                                                                                                                                                                                                                                                                                                                     | -        | When true, the input will automatically display success/error state based on `:user-valid` and `:user-invalid` styles                                                                                              |
