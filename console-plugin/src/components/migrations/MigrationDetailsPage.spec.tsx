import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { k8sPatch, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import MigrationDetailsPage, { stepIndex } from './MigrationDetailsPage';

const mockWatch = useK8sWatchResource as jest.Mock;
const mockPatch = k8sPatch as jest.Mock;

const migration = (phase: string, extra = {}) => ({
  metadata: { name: 'web01', namespace: 'apps' },
  spec: { sourceRef: { name: 'web01-server' }, disks: [], ...extra },
  status: { phase, vmName: 'web01', message: `now ${phase}` },
});

const renderPage = () =>
  render(
    <MemoryRouter initialEntries={['/p2v/ns/apps/migrations/web01']}>
      <Routes>
        <Route path="/p2v/ns/:ns/migrations/:name" element={<MigrationDetailsPage />} />
      </Routes>
    </MemoryRouter>,
  );

describe('MigrationDetailsPage', () => {
  beforeEach(() => mockPatch.mockReset());

  it('offers cut over and roll back while testing, and links to the VM console', () => {
    mockWatch.mockReturnValue([migration('Testing'), true, undefined]);
    renderPage();
    expect(screen.getByRole('button', { name: 'Cut over' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Roll back' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Archive' })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open the VM console' })).toHaveAttribute(
      'href',
      '/k8s/ns/apps/kubevirt.io~v1~VirtualMachine/web01/console',
    );
  });

  it('asks for confirmation before cutting over, then sets spec.cutover', async () => {
    mockWatch.mockReturnValue([migration('Testing'), true, undefined]);
    mockPatch.mockResolvedValue({});
    renderPage();
    fireEvent.click(screen.getByRole('button', { name: 'Cut over' }));
    expect(screen.getByText('Cut over to the virtual machine?')).toBeInTheDocument();
    expect(mockPatch).not.toHaveBeenCalled();
    const buttons = screen.getAllByRole('button', { name: 'Cut over' });
    fireEvent.click(buttons[buttons.length - 1]);
    await waitFor(() => {
      expect(screen.queryByText('Cut over to the virtual machine?')).not.toBeInTheDocument();
    });
    expect(mockPatch).toHaveBeenCalledTimes(1);
    const arg = (mockPatch.mock.calls[0] as [{ data: { path: string }[] }])[0];
    expect(arg.data[0].path).toBe('/spec/cutover');
  });

  it('offers archive and roll back when completed, nothing while copying', () => {
    mockWatch.mockReturnValue([migration('Completed'), true, undefined]);
    const { unmount } = renderPage();
    expect(screen.getByRole('button', { name: 'Archive' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Roll back' })).toBeInTheDocument();
    unmount();
    mockWatch.mockReturnValue([migration('Copying'), true, undefined]);
    renderPage();
    expect(
      screen.queryByRole('button', { name: /Cut over|Roll back|Archive/ }),
    ).not.toBeInTheDocument();
  });

  it('surfaces a failing step', () => {
    const m = migration('Converting');
    (m.status as Record<string, unknown>).conditions = [
      {
        type: 'Degraded',
        status: 'True',
        reason: 'ReconcileError',
        message: 'passt exited with status 1',
      },
    ];
    mockWatch.mockReturnValue([m, true, undefined]);
    renderPage();
    expect(screen.getByText('A step is failing and being retried')).toBeInTheDocument();
    expect(screen.getAllByText('passt exited with status 1').length).toBeGreaterThan(0);
  });

  it('maps phases to steps', () => {
    expect(stepIndex('Preflight')).toBe(0);
    expect(stepIndex('Copying')).toBe(2);
    expect(stepIndex('Testing')).toBe(4);
    expect(stepIndex('Completed')).toBe(6);
  });
});
