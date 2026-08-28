// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex } from '@nvidia/foundations-react-core'

export const ModelHubActions = ({
    onSubmit,
    disabled,
    count
}: {
    onSubmit: () => void
    disabled: boolean
    count: number
}) => {
    return (
        <Flex align="center" justify="end" gap="2">
            <Button onClick={() => onSubmit()} size="small" color="brand" disabled={disabled}>
                Download {count ? `(${count})` : ''}
            </Button>
        </Flex>
    )
}
