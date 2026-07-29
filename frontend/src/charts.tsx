import { Area, AreaChart, CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'

// This module is intentionally kept separate from main.tsx and only ever
// loaded via React.lazy()/dynamic import. recharts pulls in a sizeable
// amount of code, and the dashboard charts are not needed until the first
// render pass after data loads, so splitting them into their own chunk
// keeps the main bundle smaller.

const chartColors = { cpu: '#55d6ff', memory: '#9a7bff' }

type ChartPoint = { time: string; cpu: number; memory?: number }

export function NodeChart({ data }: { data: ChartPoint[] }) {
  return <ResponsiveContainer width="100%" height={150}>
    <LineChart data={data}>
      <CartesianGrid strokeDasharray="3 3" stroke="#24354a" />
      <XAxis dataKey="time" tick={{ fill: '#71839a', fontSize: 10 }} minTickGap={28} />
      <YAxis domain={[0, 100]} tick={{ fill: '#71839a', fontSize: 10 }} width={30} tickFormatter={v => `${v}%`} />
      <Tooltip contentStyle={{ background: '#101a2a', border: '1px solid #29415d', borderRadius: 6 }} />
      <Line type="monotone" dataKey="cpu" stroke={chartColors.cpu} strokeWidth={2} dot={false} />
      <Line type="monotone" dataKey="memory" stroke={chartColors.memory} strokeWidth={2} dot={false} />
    </LineChart>
  </ResponsiveContainer>
}

export function GuestChart({ data }: { data: ChartPoint[] }) {
  return <ResponsiveContainer width="100%" height="100%">
    <AreaChart data={data}>
      <Area type="monotone" dataKey="cpu" stroke="#55d6ff" fill="#55d6ff" fillOpacity={0.2} strokeWidth={2} dot={{ r: 2, strokeWidth: 0, fill: '#8de8ff' }} activeDot={{ r: 4, stroke: '#dff9ff', strokeWidth: 1, fill: '#55d6ff' }} />
    </AreaChart>
  </ResponsiveContainer>
}
