import { useEffect, useRef } from 'react'
import { codePointOffsetToUTF16 } from './remoteCursors'
import Editor, { type Monaco, type OnMount } from '@monaco-editor/react'
import type { editor } from 'monaco-editor'

interface MonacoWorkspaceProps {
  value: string
  language: string
  readOnly: boolean
  selection?: () => { anchor: number; head: number } | null
  onChange: (value: string) => void
  onMount?: (editor: editor.IStandaloneCodeEditor, monaco: Monaco, isSyncing: () => boolean) => void
}

export function MonacoWorkspace({ value, language, readOnly, selection, onChange, onMount }: MonacoWorkspaceProps) {
  const editorRef = useRef<editor.IStandaloneCodeEditor | null>(null)
  const syncing = useRef(false)
  useEffect(() => {
    const instance = editorRef.current
    const model = instance?.getModel()
    if (!instance || !model || value === model.getValue()) return
    // Monaco 整段替换正文时会折叠选区。恢复按编辑计算后的选区，并忽略此次程序更新。
    const cursor = selection?.()
    syncing.current = true
    try {
      model.applyEdits([{ range: model.getFullModelRange(), text: value }])
      if (cursor) {
        const anchor = model.getPositionAt(codePointOffsetToUTF16(value, cursor.anchor))
        const head = model.getPositionAt(codePointOffsetToUTF16(value, cursor.head))
        instance.setSelection({
          selectionStartLineNumber: anchor.lineNumber, selectionStartColumn: anchor.column,
          positionLineNumber: head.lineNumber, positionColumn: head.column,
        }, 'collab-sync')
      }
    } finally {
      syncing.current = false
    }
  }, [value, selection])
  const mount: OnMount = (instance, monaco) => {
    editorRef.current = instance
    instance.focus()
    onMount?.(instance, monaco, () => syncing.current)
  }

  return (
    <Editor
      height="100%"
      language={language}
      defaultValue={value}
      theme="vs-dark"
      onChange={(next) => {
        const nextValue = next ?? ''
        // Monaco also reports controlled value updates. Only user changes should
        // be converted into collaborative operations.
        if (!syncing.current && nextValue !== value) onChange(nextValue)
      }}
      onMount={mount}
      loading={<div className="editor-loading"><span className="spinner" /> Loading Monaco…</div>}
      options={{
        readOnly,
        minimap: { enabled: true, scale: 0.8 },
        fontFamily: "'JetBrains Mono', 'SFMono-Regular', Consolas, monospace",
        fontSize: 14,
        lineHeight: 23,
        padding: { top: 18 },
        smoothScrolling: true,
        cursorSmoothCaretAnimation: 'on',
        renderLineHighlight: 'gutter',
        bracketPairColorization: { enabled: true },
        automaticLayout: true,
        scrollBeyondLastLine: false,
        wordWrap: 'off',
      }}
    />
  )
}
