import { useQuery } from "react-query";
import { Utilities } from "../../../utilities/utilities";

interface ResponseError {
    status?: number;
  }
  

const getSnapshotSettings = async (
) => {
  const res = await fetch(
    `${process.env.API_ENDPOINT}/snapshots/settings`,
    {
      headers: {
        Authorization: Utilities.getToken(),
        "Content-Type": "application/json",
      },
      method: "GET",
    }
  );
  const response = await res.json();
  console.log(response,'res')
  if (!res.ok && res.status !== 200) {
    const error = new Error('could not create a snapshot') as ResponseError
   error.status = res.status;
   throw error;
  }


  return response;
};

export const useSnapshotSettings = (
  onSuccess: (data: any) => void,
  onError: (error: Error) => void,
) => {


  return useQuery({
    queryFn: () => getSnapshotSettings(),
    queryKey: ["getSnapshotSettings"],
    onSuccess,
    onError,
  });
};

export default { useSnapshotSettings };
