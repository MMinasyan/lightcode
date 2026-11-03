// The one stateless native-dialog action: showModal on mount and close on
// destroy, so a destroyed dialog never leaves the top layer. Data reads stay
// in each component's own mount hook.
export function openDialog(node: HTMLDialogElement) {
  node.showModal();
  return {
    destroy() {
      if (node.open) node.close();
    },
  };
}
