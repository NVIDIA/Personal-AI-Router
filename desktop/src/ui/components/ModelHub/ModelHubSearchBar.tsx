// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { Button, Flex, TextInput } from '@nvidia/foundations-react-core'
import { Search } from '@/ui/components/icons'
import { SortState } from '@/ui/types/model-hub'
import { ModelHubFilterSort } from './ModelHubFilterSort'

interface ModelHubSearchBarProps {
    onSubmit: (query: string) => void
    sort: SortState
    onSort: (sort: SortState) => void
    onMenuOpenChange?: (open: boolean) => void
}

export const ModelHubSearchBar = ({
    onSubmit,
    sort,
    onSort,
    onMenuOpenChange
}: ModelHubSearchBarProps) => {
    const [query, setQuery] = useState('')

    const handleValueChange = (value: string) => {
        setQuery(value)
        // Results are cached in the renderer, so filter live as the user types.
        onSubmit(value)
    }

    const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
        if (e.key === 'Enter') {
            onSubmit(query)
        }
    }

    return (
        <Flex align="center" gap="1" className="min-w-0 w-full">
            <ModelHubFilterSort sort={sort} onSort={onSort} onMenuOpenChange={onMenuOpenChange} />
            <TextInput
                placeholder="Search for a model"
                value={query}
                onKeyDown={handleKeyDown}
                onValueChange={handleValueChange}
                className="flex-1 min-w-0 max-h-[32px]"
            />
            <Button
                onClick={() => onSubmit(query)}
                kind="secondary"
                size="small"
                className="shrink-0"
                aria-label="Search models"
            >
                <Search style={{ fontSize: 14 }} />
            </Button>
        </Flex>
    )
}
