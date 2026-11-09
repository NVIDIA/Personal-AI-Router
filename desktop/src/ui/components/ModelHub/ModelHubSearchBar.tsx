// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex, TextInput } from '@nvidia/foundations-react-core'
import { Search } from '@/ui/components/icons'
import { SortState } from '@/ui/types/model-hub'
import { ModelHubFilterSort } from './ModelHubFilterSort'

interface ModelHubSearchBarProps {
    query: string
    searching: boolean
    onQueryChange: (query: string) => void
    onSearch: (query: string) => void
    sort: SortState
    onSort: (sort: SortState) => void
    onMenuOpenChange?: (open: boolean) => void
}

export const ModelHubSearchBar = ({
    query,
    searching,
    onQueryChange,
    onSearch,
    sort,
    onSort,
    onMenuOpenChange
}: ModelHubSearchBarProps) => {
    const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
        if (e.key === 'Enter' && !searching && query.trim()) {
            e.preventDefault()
            onSearch(query)
        }
    }

    return (
        <Flex align="center" gap="1" className="min-w-0 w-full">
            <ModelHubFilterSort sort={sort} onSort={onSort} onMenuOpenChange={onMenuOpenChange} />
            <TextInput
                placeholder="Search for a model"
                value={query}
                onKeyDown={handleKeyDown}
                onValueChange={onQueryChange}
                className="flex-1 min-w-0 max-h-[32px]"
            />
            <Button
                onClick={() => onSearch(query)}
                kind="secondary"
                size="small"
                className="shrink-0"
                disabled={searching || query.trim().length === 0}
                aria-busy={searching}
                aria-label={searching ? 'Searching models' : 'Search models'}
            >
                {searching ? (
                    <span
                        className="spinner-element"
                        role="status"
                        aria-label="Searching..."
                        style={{ width: 14, height: 14 }}
                    />
                ) : (
                    <Search style={{ fontSize: 14 }} />
                )}
            </Button>
        </Flex>
    )
}
