// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import './NodeActive.css'

export default function NodeActive({ show, color }: { show: boolean; color: string }) {
    const borderColor = useMemo(() => {
        const first = `${color}87`
        const second = `${color}30`
        const third = `${color}75`
        const fourth = `${color}35`
        return {
            first,
            second,
            third,
            fourth
        }
    }, [color])

    const activeShadow = useMemo(() => {
        return `0 0 2px 1px ${borderColor.first}, 0 0 15px ${borderColor.second}, 0 0 30px ${borderColor.third}, 0 0 60px ${borderColor.fourth}`
    }, [borderColor])

    const ringClass = useMemo(() => {
        return `ring-item`
    }, [])

    const style = useMemo(() => {
        return {
            // borderColor: color
            color
        }
    }, [color])

    const glowStyle = useMemo(() => {
        return {
            boxShadow: activeShadow
        }
    }, [activeShadow])

    return (
        <div
            className={`ring-outer-container transition-opacity duration-300`}
            style={{ opacity: show ? 1 : 0 }}
        >
            <div className="ring-inner-container">
                <div className="ring-glow" style={glowStyle} />
                <div className={ringClass} style={style} data-index="0" />
                <div className={ringClass} style={style} data-index="1" />
                <div className={ringClass} style={style} data-index="2" />
                <div className={ringClass} style={style} data-index="3" />
                <div className={ringClass} style={style} data-index="4" />
                <div className={ringClass} style={style} data-index="5" />
            </div>
        </div>
    )
}
