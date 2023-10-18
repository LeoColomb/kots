import classNames from "classnames";
import MaterialReactTable from "material-react-table";
import React, { ChangeEvent, useMemo, useReducer, useState } from "react";
import Modal from "react-modal";
import { useQuery } from "react-query";
import { Link, useParams } from "react-router-dom";

import { KotsPageTitle } from "@components/Head";
import { useApps } from "@features/App";
import { rbacRoles } from "../../constants/rbac";
import { Utilities } from "../../utilities/utilities";
import Icon from "../Icon";
import CodeSnippet from "../shared/CodeSnippet";

import "@src/scss/components/apps/HelmVMClusterManagement.scss";

const testData = {
  isHelmVMEnabled: true,
  ha: false,
  nodes: [
    {
      name: "test-helmvm-node",
      isConnected: true,
      isReady: true,
      isPrimaryNode: true,
      canDelete: false,
      kubeletVersion: "v1.28.2",
      cpu: {
        capacity: 8,
        available: 7.466876775,
      },
      memory: {
        capacity: 31.33294677734375,
        available: 24.23790740966797,
      },
      pods: {
        capacity: 110,
        available: 77,
      },
      labels: [
        "beta.kubernetes.io/arch:amd64",
        "beta.kubernetes.io/os:linux",
        "node-role.kubernetes.io/master:",
        "node.kubernetes.io/exclude-from-external-load-balancers:",
        "kubernetes.io/arch:amd64",
        "kubernetes.io/hostname:laverya-kurl",
        "kubernetes.io/os:linux",
        "node-role.kubernetes.io/control-plane:",
      ],
      conditions: {
        memoryPressure: false,
        diskPressure: false,
        pidPressure: false,
        ready: true,
      },
    },
    {
      name: "test-helmvm-worker",
      isConnected: true,
      isReady: true,
      isPrimaryNode: false,
      canDelete: false,
      kubeletVersion: "v1.28.2",
      cpu: {
        capacity: 4,
        available: 3.761070507,
      },
      memory: {
        capacity: 15.50936508178711,
        available: 11.742542266845703,
      },
      pods: {
        capacity: 110,
        available: 94,
      },
      labels: [
        "beta.kubernetes.io/arch:amd64",
        "beta.kubernetes.io/os:linux",
        "kubernetes.io/arch:amd64",
        "kubernetes.io/os:linux",
        "kurl.sh/cluster:true",
      ],
      conditions: {
        memoryPressure: false,
        diskPressure: false,
        pidPressure: false,
        ready: true,
      },
    },
  ],
};

type State = {
  displayAddNode: boolean;
  confirmDeleteNode: string;
  deleteNodeError: string;
  showConfirmDrainModal: boolean;
  nodeNameToDrain: string;
  drainingNodeName: string | null;
  drainNodeSuccessful: boolean;
};

const HelmVMClusterManagement = ({
  fromLicenseFlow = false,
  appName,
}: {
  fromLicenseFlow?: boolean;
  appName?: string;
}) => {
  const [state, setState] = useReducer(
    (prevState: State, newState: Partial<State>) => ({
      ...prevState,
      ...newState,
    }),
    {
      displayAddNode: false,
      confirmDeleteNode: "",
      deleteNodeError: "",
      showConfirmDrainModal: false,
      nodeNameToDrain: "",
      drainingNodeName: null,
      drainNodeSuccessful: false,
    }
  );
  const [selectedNodeTypes, setSelectedNodeTypes] = useState<string[]>([]);

  const { data: appsData } = useApps();
  const app = appsData?.apps?.find((a) => a.name === appName);
  const { slug } = useParams();

  // #region queries
  type NodesResponse = {
    ha: boolean;
    isHelmVMEnabled: boolean;
    nodes: {
      name: string;
      isConnected: boolean;
      isReady: boolean;
      isPrimaryNode: boolean;
      canDelete: boolean;
      kubeletVersion: string;
      cpu: {
        capacity: number;
        available: number;
      };
      memory: {
        capacity: number;
        available: number;
      };
      pods: {
        capacity: number;
        available: number;
      };
      labels: string[];
      conditions: {
        memoryPressure: boolean;
        diskPressure: boolean;
        pidPressure: boolean;
        ready: boolean;
      };
    }[];
  };

  const {
    data: nodesData,
    isInitialLoading: nodesLoading,
    error: nodesError,
  } = useQuery<NodesResponse, Error, NodesResponse, string>({
    queryKey: "helmVmNodes",
    queryFn: async () => {
      const res = await fetch(`${process.env.API_ENDPOINT}/helmvm/nodes`, {
        headers: {
          Accept: "application/json",
        },
        credentials: "include",
        method: "GET",
      });
      if (!res.ok) {
        if (res.status === 401) {
          Utilities.logoutUser();
        }
        console.log(
          "failed to get node status list, unexpected status code",
          res.status
        );
        try {
          const error = await res.json();
          throw new Error(
            error?.error?.message || error?.error || error?.message
          );
        } catch (err) {
          throw new Error("Unable to fetch nodes, please try again later.");
        }
      }
      return res.json();
    },
    refetchInterval: (data) => (data ? 1000 : 0),
    retry: false,
  });

  type AddNodeCommandResponse = {
    command: string;
    expiry: string;
  };

  const {
    data: generateAddNodeCommand,
    isLoading: generateAddNodeCommandLoading,
    error: generateAddNodeCommandError,
  } = useQuery<AddNodeCommandResponse, Error, AddNodeCommandResponse>({
    queryKey: ["generateAddNodeCommand", selectedNodeTypes],
    queryFn: async ({ queryKey }) => {
      const [, nodeTypes] = queryKey;
      const res = await fetch(
        `${process.env.API_ENDPOINT}/helmvm/generate-node-join-command`,
        {
          headers: {
            "Content-Type": "application/json",
            Accept: "application/json",
          },
          credentials: "include",
          method: "POST",
          body: JSON.stringify({
            roles: nodeTypes,
          }),
        }
      );
      if (!res.ok) {
        if (res.status === 401) {
          Utilities.logoutUser();
        }
        console.log(
          "failed to get generate node command, unexpected status code",
          res.status
        );
        try {
          const error = await res.json();
          throw new Error(
            error?.error?.message || error?.error || error?.message
          );
        } catch (err) {
          throw new Error(
            "Unable to generate node join command, please try again later."
          );
        }
      }
      return res.json();
    },
    enabled: selectedNodeTypes.length > 0,
  });

  // TODO: import useMutation
  // const {
  //   mutate: addNodeType,
  //   isLoading: addNodeTypeLoading,
  //   error: addNodeTypeError,
  // } = useMutation({
  //   mutationFn: async () => {
  //     return (
  //       await fetch(`${process.env.API_ENDPOINT}/helmvm/nodes`, {
  //         headers: {
  //           "Content-Type": "application/json",
  //           Accept: "application/json",
  //         },
  //         credentials: "include",
  //         method: "POST",
  //       })
  //     ).json();
  //   },
  // });
  // #endregion

  const onAddNodeClick = () => {
    setState({
      displayAddNode: true,
    });
  };

  // #region node type logic
  const NODE_TYPES = ["controller"];

  const determineDisabledState = () => {
    // if (nodeType === "controller") {
    //   const numControllers = testData.nodes.reduce((acc, node) => {
    //     if (node.labels.includes("controller")) {
    //       acc += 1;
    //     }
    //     return acc;
    //   }, 0);
    //   return numControllers === 3;
    // }
    return false;
  };

  const handleSelectNodeType = (e: ChangeEvent<HTMLInputElement>) => {
    let nodeType = e.currentTarget.value;
    let types = selectedNodeTypes;

    if (selectedNodeTypes.includes(nodeType)) {
      setSelectedNodeTypes(types.filter((type) => type !== nodeType));
    } else {
      setSelectedNodeTypes([...types, nodeType]);
    }
  };
  // #endregion

  const columns = useMemo(
    () => [
      {
        accessorKey: "name",
        header: "Name",
        enableHiding: false,
        enableColumnDragging: false,
        size: 150,
      },
      {
        accessorKey: "roles",
        header: "Role(s)",
        size: 404,
      },
      {
        accessorKey: "status",
        header: "Status",
        size: 150,
      },
      {
        accessorKey: "disk",
        header: "Disk",
        size: 150,
      },
      {
        accessorKey: "cpu",
        header: "CPU",
        size: 150,
      },
      {
        accessorKey: "memory",
        header: "Memory",
        size: 150,
      },
      {
        accessorKey: "pause",
        header: "Pause",
        size: 100,
      },
      {
        accessorKey: "delete",
        header: "Delete",
        size: 100,
      },
    ],
    []
  );

  const mappedNodes = useMemo(() => {
    return (nodesData?.nodes || testData.nodes).map((n) => ({
      name: slug ? (
        <Link
          to={`${slug}/cluster/${n.name}`}
          className="tw-font-semibold tw-text-blue-300 hover:tw-underline"
        />
      ) : (
        n.name
      ),
      roles: (
        <div className="tw-w-full tw-flex tw-flex-wrap tw-gap-1">
          {n.labels.map((l) => (
            <span className="tw-font-semibold tw-text-xs tw-px-1 tw-rounded-sm tw-border tw-border-solid tw-bg-white tw-border-gray-100">
              {l}
            </span>
          ))}
        </div>
      ),
      status: n.isReady ? "Ready" : "Not Ready",
      disk: n.conditions.diskPressure ? "Disk Pressure" : "No Disk Pressure",
      cpu: n.conditions.pidPressure ? "CPU Pressure" : "No CPU Pressure",
      memory: n.conditions.memoryPressure
        ? "Memory Pressure"
        : "No Memory Pressure",
      pause: (
        <>
          <button className="btn secondary">Pause</button>
        </>
      ),
      delete: (
        <>
          <button className="btn red primary">Delete</button>
        </>
      ),
    }));
  }, [nodesData?.nodes?.toString()]);
  // #endregion

  return (
    <div className="HelmVMClusterManagement--wrapper container flex-column flex1 u-overflow--auto u-paddingTop--50 tw-font-sans">
      <KotsPageTitle pageName="Cluster Management" />
      <div className="flex-column flex1 alignItems--center u-paddingBottom--50">
        <div className="centered-container tw-mb-10 tw-pb-6 tw-flex tw-flex-col tw-gap-4">
          <p className="flex-auto u-fontSize--larger u-fontWeight--bold u-textColor--primary">
            Cluster Nodes
          </p>
          <div className="tw-flex tw-gap-6 tw-items-center">
            <p className="tw-text-base tw-flex-1 tw-text-gray-600">
              This page lists the nodes that are configured and shows the
              status/health of each.
            </p>
            {Utilities.sessionRolesHasOneOf([rbacRoles.CLUSTER_ADMIN]) && (
              <button
                className="btn primary tw-ml-auto tw-w-fit tw-h-fit"
                onClick={onAddNodeClick}
              >
                Add node
              </button>
            )}
          </div>
          <div className="flex1 u-overflow--auto">
            {nodesLoading && (
              <p className="tw-text-base tw-w-full tw-text-center tw-py-4 tw-text-gray-500 tw-font-semibold">
                Loading nodes...
              </p>
            )}
            {!nodesData && nodesError && (
              <p className="tw-text-base tw-w-full tw-text-center tw-py-4 tw-text-pink-500 tw-font-semibold">
                {nodesError?.message}
              </p>
            )}
            {(nodesData?.nodes || testData?.nodes) && (
              <MaterialReactTable
                columns={columns}
                data={mappedNodes}
                state={{
                  columnPinning: { left: ["name"] },
                }}
                enableColumnResizing
                enableColumnActions={false}
                enableColumnOrdering
                enableBottomToolbar={false}
                muiTableHeadProps={{
                  sx: {
                    "& hr": {
                      width: "0",
                    },
                  },
                }}
                muiTableBodyProps={{
                  sx: {
                    "& tr:nth-of-type(odd)": {
                      backgroundColor: "#f5f5f5",
                    },
                  },
                }}
                muiTableBodyCellProps={{
                  sx: {
                    borderRight: "2px solid #e0e0e0",
                  },
                }}
                muiTablePaperProps={{
                  sx: {
                    width: "100%",
                    boxShadow: "none",
                  },
                }}
                initialState={{ density: "compact" }}
                enablePagination={false}
                enableColumnFilters={false}
              />
            )}
          </div>
          {fromLicenseFlow && (
            <Link
              className="btn primary tw-w-fit tw-ml-auto"
              to={
                app?.isConfigurable
                  ? `/${app?.slug}/config`
                  : `/app/${app?.slug}`
              }
            >
              Continue
            </Link>
          )}
        </div>
      </div>
      {/* MODALS */}
      <Modal
        isOpen={state.displayAddNode}
        onRequestClose={() => setState({ displayAddNode: false })}
        contentLabel="Add Node"
        className="Modal"
        ariaHideApp={false}
      >
        <div className="Modal-body tw-flex tw-flex-col tw-gap-4 tw-font-sans">
          <div className="tw-flex">
            <h1 className="u-fontSize--largest u-fontWeight--bold u-textColor--primary u-lineHeight--normal u-marginBottom--more">
              Add a Node
            </h1>
            <Icon
              icon="close"
              size={14}
              className="tw-ml-auto gray-color clickable close-icon"
              onClick={() => setState({ displayAddNode: false })}
            />
          </div>
          <p className="tw-text-base tw-text-gray-600">
            To add a node to this cluster, select the type of node you'd like to
            add. Once you've selected a node type, we will generate a node join
            command for you to use in the CLI. When the node successfully joins
            the cluster, you will see it appear in the list of nodes on this
            page.
          </p>
          <div className="tw-grid tw-gap-2 tw-grid-cols-4 tw-auto-rows-auto">
            {NODE_TYPES.map((nodeType) => (
              <div
                key={nodeType}
                className={classNames("BoxedCheckbox", {
                  "is-active": selectedNodeTypes.includes(nodeType),
                  "is-disabled": determineDisabledState(),
                })}
              >
                <input
                  id={`${nodeType}NodeType`}
                  className="u-cursor--pointer hidden-input"
                  type="checkbox"
                  name={`${nodeType}NodeType`}
                  value={nodeType}
                  disabled={determineDisabledState()}
                  checked={selectedNodeTypes.includes(nodeType)}
                  onChange={handleSelectNodeType}
                />
                <label
                  htmlFor={`${nodeType}NodeType`}
                  className="tw-block u-cursor--pointer u-userSelect--none u-textColor--primary u-fontSize--normal u-fontWeight--medium tw-text-center"
                >
                  {nodeType === "controller" ? "controlplane" : nodeType}
                </label>
              </div>
            ))}
          </div>
          <div>
            {generateAddNodeCommandLoading && (
              <p className="tw-text-base tw-w-full tw-text-center tw-py-4 tw-text-gray-500 tw-font-semibold">
                Generating command...
              </p>
            )}
            {!generateAddNodeCommand && generateAddNodeCommandError && (
              <p className="tw-text-base tw-w-full tw-text-center tw-py-4 tw-text-pink-500 tw-font-semibold">
                {generateAddNodeCommandError?.message}
              </p>
            )}
            {!generateAddNodeCommandLoading && generateAddNodeCommand?.command && (
              <CodeSnippet
                key={selectedNodeTypes.toString()}
                language="bash"
                canCopy={true}
                onCopyText={
                  <span className="u-textColor--success">Copied!</span>
                }
              >
                {generateAddNodeCommand?.command || ""}
              </CodeSnippet>
            )}
          </div>
          {/* buttons */}
          <div className="tw-w-full tw-flex tw-justify-end tw-gap-2">
            <button
              className="btn secondary large"
              onClick={() => setState({ displayAddNode: false })}
            >
              Close
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
};

export default HelmVMClusterManagement;
